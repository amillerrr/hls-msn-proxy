// Package proxy provides the HTTP reverse proxy that intercepts HLS playlist
// responses and applies MSN correction.
//
// Origin selection: consistent hashing on stream key ensures sequential playlist
// fetches for the same rendition hit the same upstream packager. This prevents
// segment content mismatches where different packagers serve different segment
// URIs for the same timeline position. Failover to alternate origins happens
// only when the pinned origin returns a 5xx or is unreachable.
//
// Fail-closed behavior:
//   - Upstream success + state available → corrected playlist
//   - Upstream success + no state (ErrNoState) → serve stale if available, else 503
//   - Upstream failure → serve stale if available, else 502
//   - Non-m3u8 requests → direct passthrough (no interception)
package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/amillerrr/hls-msn-proxy/internal/rewriter"
	"github.com/amillerrr/hls-msn-proxy/internal/state"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics
var (
	playlistsProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_playlists_processed_total",
		Help: "Total media playlists processed",
	})
	regressionsFixed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_regressions_fixed_total",
		Help: "Total MSN regressions corrected",
	})
	staleServed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_stale_served_total",
		Help: "Total stale playlists served",
	})
	failClosed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_fail_closed_total",
		Help: "Total requests failed closed (503) due to no state",
	})
	upstreamErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_upstream_errors_total",
		Help: "Total upstream fetch failures",
	})
	passthroughCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_passthrough_total",
		Help: "Total playlists passed through unmodified (master/VOD)",
	})
	redisSource = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_state_source_redis_total",
		Help: "State reads from Redis",
	})
	localSource = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_state_source_local_total",
		Help: "State reads from local cache",
	})
	baselineSource = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_state_source_baseline_total",
		Help: "New baseline states established",
	})
	originFailovers = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_origin_failovers_total",
		Help: "Times a request failed over to an alternate origin",
	})
	offsetExcessive = promauto.NewCounter(prometheus.CounterOpts{
		Name: "msn_proxy_offset_excessive_total",
		Help: "Corrections where offset exceeded safe threshold",
	})
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "msn_proxy_request_duration_seconds",
		Help:    "Request duration in seconds",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
	}, []string{"type"}) // type: "playlist", "segment", "other"
)

// staleEntry holds a cached playlist with its timestamp.
type staleEntry struct {
	body      []byte
	timestamp time.Time
}

// Config for the proxy.
type Config struct {
	Upstreams       []string      // Upstream origin URLs
	StaleTTL        time.Duration // How long stale playlists remain valid
	UpstreamTimeout time.Duration // Timeout per upstream request
}

func DefaultConfig() Config {
	return Config{
		StaleTTL:        90 * time.Second, // Covers Redis failover window (30-60s) with margin
		UpstreamTimeout: 3 * time.Second,  // Under MediaTailor's default 4s origin timeout
	}
}

// Proxy is the MSN-correcting reverse proxy.
type Proxy struct {
	state  *state.Manager
	cfg    Config
	logger *slog.Logger

	reverseProxies []*httputil.ReverseProxy

	// Global round-robin counter for non-playlist requests (segments, etc.)
	// where origin pinning doesn't matter since segment content is immutable.
	passthroughOrigin int
	passthroughMu     sync.Mutex

	staleCache sync.Map // uri → *staleEntry
}

// New creates a proxy with the given state manager and config.
func New(stateMgr *state.Manager, cfg Config, logger *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		state:  stateMgr,
		cfg:    cfg,
		logger: logger,
	}

	for _, upstream := range cfg.Upstreams {
		u, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream %q: %w", upstream, err)
		}

		rp := httputil.NewSingleHostReverseProxy(u)
		rp.Transport = &http.Transport{
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: cfg.UpstreamTimeout,
			DisableCompression:    true, // We need the raw body to rewrite
		}
		// Suppress default error logging (we handle errors ourselves)
		rp.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelWarn)

		p.reverseProxies = append(p.reverseProxies, rp)
	}

	if len(p.reverseProxies) == 0 {
		return nil, fmt.Errorf("at least one upstream required")
	}

	return p, nil
}

// ServeHTTP handles all incoming requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		p.handlePlaylist(w, r)
		requestDuration.WithLabelValues("playlist").Observe(time.Since(start).Seconds())
	} else if isSegment(r.URL.Path) {
		p.handlePassthrough(w, r)
		requestDuration.WithLabelValues("segment").Observe(time.Since(start).Seconds())
	} else {
		p.handlePassthrough(w, r)
		requestDuration.WithLabelValues("other").Observe(time.Since(start).Seconds())
	}
}

// handlePlaylist fetches upstream, applies MSN correction, caches result.
func (p *Proxy) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Path
	streamKey := cleanStreamKey(uri)

	// Fetch from upstream using origin pinning (consistent hash on stream key)
	body, statusCode, err := p.fetchUpstreamPinned(r, streamKey)
	if err != nil || statusCode >= 500 {
		upstreamErrors.Inc()
		p.logger.Warn("upstream failed, trying stale",
			"uri", uri, "error", err, "status", statusCode)
		p.serveStaleOrFail(w, uri, 502)
		return
	}

	if statusCode == http.StatusNotFound {
		http.NotFound(w, r)
		return
	}

	if statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
		w.Write(body)
		return
	}

	// Parse playlist
	parsed := rewriter.Parse(body)

	// Master playlists and VOD: pass through unmodified
	if !parsed.IsMedia || parsed.IsVOD {
		passthroughCount.Inc()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("X-MSN-Proxy", "passthrough")
		w.Write(body)
		return
	}

	// Compute correction (Redis pipeline or local fallback)
	result, err := p.state.CorrectAndUpdate(r.Context(), streamKey, parsed)
	if err != nil {
		if err == state.ErrNoState {
			// No state available and Redis is down. We cannot verify correctness.
			// Serve stale if we have it; otherwise fail closed.
			p.logger.Error("no state available, failing closed",
				"uri", uri, "error", err)
			failClosed.Inc()
			p.serveStaleOrFail(w, uri, 503)
			return
		}
		// Unexpected error — also fail closed
		p.logger.Error("state error, failing closed",
			"uri", uri, "error", err)
		failClosed.Inc()
		p.serveStaleOrFail(w, uri, 503)
		return
	}

	// Track state source
	switch result.Source {
	case "redis":
		redisSource.Inc()
	case "local":
		localSource.Inc()
	case "baseline":
		baselineSource.Inc()
	}

	playlistsProcessed.Inc()

	// Apply correction to playlist body
	corrected := rewriter.Apply(body, result.Correction)

	if result.Correction.WasRegression {
		regressionsFixed.Inc()
		p.logger.Info("MSN regression corrected",
			"stream", streamKey,
			"original_msn", result.Correction.OriginalMSN,
			"corrected_msn", result.Correction.CorrectedMSN,
			"offset", result.Correction.OffsetApplied,
			"source", result.Source,
		)
	}

	if result.Correction.OffsetExcessive {
		offsetExcessive.Inc()
		p.logger.Error("MSN offset exceeds safe threshold",
			"stream", streamKey,
			"offset", result.Correction.OffsetApplied,
			"threshold", rewriter.MaxReasonableOffset,
		)
	}

	// Cache for stale serving
	p.staleCache.Store(uri, &staleEntry{
		body:      corrected,
		timestamp: time.Now(),
	})

	// Serve the corrected playlist with diagnostic headers
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-MSN-Proxy", "active")
	w.Header().Set("X-MSN-Source", result.Source)
	w.Header().Set("X-MSN-Original", fmt.Sprintf("%d", result.Correction.OriginalMSN))
	w.Header().Set("X-MSN-Corrected", fmt.Sprintf("%d", result.Correction.CorrectedMSN))
	w.Header().Set("X-MSN-Offset", fmt.Sprintf("%d", result.Correction.OffsetApplied))
	if result.Correction.WasRegression {
		w.Header().Set("X-MSN-Regression", "true")
	}
	w.Write(corrected)
}

// serveStaleOrFail tries the stale cache. If nothing is cached or it's expired,
// returns the given error status code.
func (p *Proxy) serveStaleOrFail(w http.ResponseWriter, uri string, errorCode int) {
	val, ok := p.staleCache.Load(uri)
	if !ok {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(errorCode)
		fmt.Fprintf(w, "no playlist available (code %d)", errorCode)
		return
	}

	entry := val.(*staleEntry)
	age := time.Since(entry.timestamp)

	if age > p.cfg.StaleTTL {
		p.staleCache.Delete(uri)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(errorCode)
		fmt.Fprintf(w, "stale playlist expired (age: %s)", age.Round(time.Second))
		return
	}

	staleServed.Inc()
	p.logger.Warn("serving stale playlist",
		"uri", uri, "age", age.Round(time.Millisecond))

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-MSN-Proxy", "stale")
	w.Header().Set("X-MSN-Stale-Age", age.Round(time.Millisecond).String())
	w.Write(entry.body)
}

// fetchUpstreamPinned fetches a playlist from the origin pinned to the given
// stream key (consistent hash). If the pinned origin fails, tries remaining
// origins in order before giving up.
func (p *Proxy) fetchUpstreamPinned(r *http.Request, streamKey string) (body []byte, status int, err error) {
	n := len(p.reverseProxies)
	primary := p.originIndexForStream(streamKey)

	for attempt := 0; attempt < n; attempt++ {
		idx := (primary + attempt) % n
		rp := p.reverseProxies[idx]

		rec := &responseRecorder{
			header: make(http.Header),
			status: 200, // default — Go's http may not call WriteHeader explicitly
		}

		rp.ServeHTTP(rec, r)

		if rec.status >= 500 {
			err = fmt.Errorf("upstream[%d] returned %d", idx, rec.status)
			if attempt > 0 {
				originFailovers.Inc()
			}
			continue // try next origin
		}

		if attempt > 0 {
			originFailovers.Inc()
			p.logger.Warn("origin failover succeeded",
				"stream", streamKey,
				"primary_idx", primary,
				"used_idx", idx,
				"attempt", attempt+1,
			)
		}

		respBody := rec.body.Bytes()

		// Safety: decompress if upstream sent gzip despite DisableCompression
		if rec.header.Get("Content-Encoding") == "gzip" && len(respBody) > 2 {
			gr, gzErr := gzip.NewReader(bytes.NewReader(respBody))
			if gzErr == nil {
				decompressed, readErr := io.ReadAll(gr)
				gr.Close()
				if readErr == nil {
					respBody = decompressed
				}
			}
		}

		return respBody, rec.status, nil
	}

	return nil, 0, fmt.Errorf("all upstreams failed: %w", err)
}

// originIndexForStream returns a stable origin index for a given stream key
// using consistent hashing (FNV-1a). This ensures consecutive playlist fetches
// for the same rendition go to the same packager, preventing segment content
// mismatches when packagers produce segments with different URIs/cut points.
func (p *Proxy) originIndexForStream(streamKey string) int {
	h := fnv.New32a()
	h.Write([]byte(streamKey))
	return int(h.Sum32()) % len(p.reverseProxies)
}

// handlePassthrough proxies non-playlist content directly (segments, keys, etc.).
// Uses global round-robin since segment content is immutable once created.
func (p *Proxy) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	p.passthroughMu.Lock()
	idx := p.passthroughOrigin % len(p.reverseProxies)
	p.passthroughOrigin++
	p.passthroughMu.Unlock()

	p.reverseProxies[idx].ServeHTTP(w, r)
}

// responseRecorder captures the upstream response for modification.
type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) WriteHeader(status int)      { r.status = status }
func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// cleanStreamKey normalizes the URI into a cache key.
func cleanStreamKey(uri string) string {
	// Strip query string
	if idx := strings.IndexByte(uri, '?'); idx >= 0 {
		uri = uri[:idx]
	}
	return strings.TrimRight(uri, "/")
}

func isSegment(path string) bool {
	suffixes := []string{".ts", ".m4s", ".mp4", ".m4a", ".m4v", ".aac", ".vtt", ".webvtt"}
	for _, s := range suffixes {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// PurgeStaleCache removes expired entries. Call periodically.
func (p *Proxy) PurgeStaleCache() int {
	purged := 0
	cutoff := time.Now().Add(-p.cfg.StaleTTL)
	p.staleCache.Range(func(key, val any) bool {
		entry := val.(*staleEntry)
		if entry.timestamp.Before(cutoff) {
			p.staleCache.Delete(key)
			purged++
		}
		return true
	})
	return purged
}

// Ensure responseRecorder implements http.ResponseWriter
var _ http.ResponseWriter = (*responseRecorder)(nil)
