// HLS MSN Proxy — Ensures monotonically increasing EXT-X-MEDIA-SEQUENCE values.
//
// Architecture: Go reverse proxy + Redis (ElastiCache) for cross-instance state.
// Designed for 24/7 FAST platforms behind AWS Elemental MediaTailor (SSAI).
//
// Fail-closed: never serves a playlist with a potentially regressed MSN.
// If state is unavailable, serves last known correct playlist or returns 503.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/amillerrr/hls-msn-proxy/internal/proxy"
	"github.com/amillerrr/hls-msn-proxy/internal/state"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	}))
	slog.SetDefault(logger)

	cfg := loadConfig()
	logger.Info("starting HLS MSN proxy",
		"listen", cfg.ListenAddr,
		"upstreams", cfg.Upstreams,
		"redis", cfg.RedisAddr,
	)

	// ---------------------------------------------------------------
	// State manager (Redis + local cache)
	// ---------------------------------------------------------------
	stateCfg := state.DefaultConfig()
	stateCfg.RedisAddr = cfg.RedisAddr
	stateMgr := state.New(stateCfg, logger)
	defer stateMgr.Close()

	// Verify Redis on startup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if stateMgr.RedisHealthy(ctx) {
		logger.Info("redis connected", "addr", cfg.RedisAddr)
	} else if cfg.RedisAddr != "" {
		logger.Warn("redis NOT reachable at startup — will retry on each request",
			"addr", cfg.RedisAddr)
	} else {
		logger.Warn("redis not configured — running in local-only mode (single instance)")
	}
	cancel()

	// ---------------------------------------------------------------
	// Proxy
	// ---------------------------------------------------------------
	proxyCfg := proxy.DefaultConfig()
	proxyCfg.Upstreams = cfg.Upstreams
	proxyCfg.StaleTTL = cfg.StaleTTL

	p, err := proxy.New(stateMgr, proxyCfg, logger)
	if err != nil {
		logger.Error("failed to create proxy", "error", err)
		os.Exit(1)
	}

	// ---------------------------------------------------------------
	// HTTP mux
	// ---------------------------------------------------------------
	mux := http.NewServeMux()

	// Health check — verifies Redis connectivity
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		redisOK := stateMgr.RedisHealthy(r.Context())
		streamCount := stateMgr.StreamCount()

		status := "healthy"
		httpCode := http.StatusOK

		// If Redis is configured but down, report degraded
		if cfg.RedisAddr != "" && !redisOK {
			status = "degraded"
			// Still return 200 so ALB keeps instance in rotation.
			// A degraded instance serving stale is better than no instance.
			// The redis_connect_errors alarm will fire separately.
		}

		json.NewEncoder(w).Encode(map[string]any{
			"status":       status,
			"redis":        redisOK,
			"stream_count": streamCount,
			"uptime":       time.Since(startTime).Round(time.Second).String(),
		})
		w.WriteHeader(httpCode)
	})

	// Deep health check — fails if Redis is down (for targeted alerting)
	mux.HandleFunc("/health/deep", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		redisOK := stateMgr.RedisHealthy(r.Context())

		if cfg.RedisAddr != "" && !redisOK {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{
				"status": "unhealthy",
				"redis":  false,
				"error":  "redis unreachable",
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"status": "healthy",
			"redis":  redisOK,
		})
	})

	// Prometheus metrics
	mux.Handle("/metrics", promhttp.Handler())

	// Stats (JSON, human-friendly)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		streamKey := r.URL.Query().Get("stream")
		if streamKey != "" {
			st, source, err := stateMgr.GetState(r.Context(), streamKey)
			if err != nil {
				json.NewEncoder(w).Encode(map[string]any{
					"error":  err.Error(),
					"stream": streamKey,
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"stream": streamKey,
				"state":  st,
				"source": source,
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"stream_count": stateMgr.StreamCount(),
			"uptime":       time.Since(startTime).Round(time.Second).String(),
			"redis":        stateMgr.RedisHealthy(r.Context()),
		})
	})

	// Admin: reset state
	mux.HandleFunc("/admin/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		streamKey := r.URL.Query().Get("stream")

		if err := stateMgr.Reset(r.Context(), streamKey); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{
				"error":  err.Error(),
				"stream": streamKey,
			})
			return
		}

		target := streamKey
		if target == "" {
			target = "all"
		}
		logger.Info("state reset", "stream", target)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"stream": target,
		})
	})

	// All other paths → proxy
	mux.Handle("/", p)

	// ---------------------------------------------------------------
	// Server
	// ---------------------------------------------------------------
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// ---------------------------------------------------------------
	// Background: stale cache purge
	// ---------------------------------------------------------------
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			purged := p.PurgeStaleCache()
			if purged > 0 {
				logger.Debug("purged stale cache entries", "count", purged)
			}
		}
	}()

	// ---------------------------------------------------------------
	// Graceful shutdown
	// ---------------------------------------------------------------
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig)
	case err := <-errCh:
		logger.Error("server error", "error", err)
	}

	// Graceful shutdown with deadline
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
	logger.Info("server stopped")
}

var startTime = time.Now()

// ---------------------------------------------------------------
// Configuration (environment variables)
// ---------------------------------------------------------------
type config struct {
	ListenAddr string
	Upstreams  []string
	RedisAddr  string
	StaleTTL   time.Duration
}

func loadConfig() config {
	cfg := config{
		ListenAddr: envOr("LISTEN_ADDR", ":8080"),
		RedisAddr:  envOr("REDIS_ADDR", ""),
		StaleTTL:   30 * time.Second,
	}

	// Parse upstreams from semicolon-separated list
	raw := envOr("UPSTREAM_ORIGINS", "")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "UPSTREAM_ORIGINS is required (semicolon-separated URLs)")
		os.Exit(1)
	}
	for _, u := range strings.Split(raw, ";") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		// Ensure scheme
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "http://" + u
		}
		cfg.Upstreams = append(cfg.Upstreams, u)
	}

	if v := envOr("STALE_TTL", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.StaleTTL = d
		}
	}

	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
