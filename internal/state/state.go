// Package state manages MSN state with Redis as primary and an in-memory
// cache as hot read layer. All Redis operations use pipelined GET/SET —
//
// The race window between read and write is negligible at this scale:
// MediaTailor polls once per target duration (~6s) per rendition, so
// concurrent writes to the same stream key effectively don't happen.
// Origin pinning further reduces this by ensuring each stream key is
// handled consistently.
//
// Fail-closed semantics:
//   - If Redis is healthy: read/write Redis, update local cache
//   - If Redis is down + local cache has state: use local (still correct for this instance)
//   - If Redis is down + no local state: return ErrNoState (caller should serve stale or 503)
//   - Never guess at state — never pass through without verification
package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/amillerrr/hls-msn-proxy/internal/rewriter"
	"github.com/redis/go-redis/v9"
)

// ErrNoState means we have no prior state for this stream and cannot verify
// MSN correctness. On a healthy cluster this only happens on the very first
// request for a new stream (which is fine — first request establishes baseline).
// If Redis is down and local cache is cold, this signals "fail closed".
var ErrNoState = errors.New("no prior state available")

// Redis key prefixes for stream state.
const (
	prefixMSN     = "msn:"
	prefixDSN     = "dsn:"
	prefixSeg     = "seg:"
	prefixOffset  = "off:"
	prefixLastSeg = "lastseg:"
)

// allPrefixes is used for SCAN-based cleanup.
var allPrefixes = []string{prefixMSN, prefixDSN, prefixSeg, prefixOffset, prefixLastSeg}

// Config for the state manager.
type Config struct {
	RedisAddr    string
	RedisDB      int
	StateTTL     time.Duration // How long MSN state persists (default: 24h for FAST)
	RedisTimeout time.Duration // Per-operation timeout
	PoolSize     int
}

func DefaultConfig() Config {
	return Config{
		StateTTL:     24 * time.Hour,
		RedisTimeout: 200 * time.Millisecond,
		PoolSize:     32,
	}
}

// Manager provides MSN state operations with fail-closed semantics.
type Manager struct {
	rdb    *redis.Client
	local  sync.Map // stream_key → *rewriter.StreamState
	cfg    Config
	logger *slog.Logger
}

// New creates a state manager. Pass empty RedisAddr to run in local-only mode
// (single-instance testing).
func New(cfg Config, logger *slog.Logger) *Manager {
	m := &Manager{
		cfg:    cfg,
		logger: logger,
	}

	if cfg.RedisAddr != "" {
		m.rdb = redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			DB:           cfg.RedisDB,
			DialTimeout:  cfg.RedisTimeout,
			ReadTimeout:  cfg.RedisTimeout,
			WriteTimeout: cfg.RedisTimeout,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: 4,
		})
	}

	return m
}

// CorrectionResult is the outcome of a correct-and-update operation.
type CorrectionResult struct {
	Correction rewriter.Correction
	NewState   rewriter.StreamState
	Source     string // "redis", "local", "baseline"
}

// CorrectAndUpdate reads prior state, computes MSN correction, and writes
// updated state. Returns the correction to apply.
//
// Fail-closed behavior:
//   - Redis healthy → pipeline read + Go compute + pipeline write
//   - Redis down + local state exists → local correction (safe for this instance)
//   - Redis down + no local state → ErrNoState (caller must serve stale or 503)
//   - First-ever request for stream → establishes baseline (no prior state to violate)
func (m *Manager) CorrectAndUpdate(ctx context.Context, streamKey string, parsed rewriter.Playlist) (*CorrectionResult, error) {
	// Try Redis first (cross-instance consistent)
	if m.rdb != nil {
		result, err := m.redisCorrect(ctx, streamKey, parsed)
		if err == nil {
			// Update local cache (hot read layer)
			m.local.Store(streamKey, &result.NewState)
			return result, nil
		}
		m.logger.Warn("redis unavailable, trying local state",
			"stream", streamKey, "error", err)
	}

	// Fall back to local state
	return m.localCorrect(streamKey, parsed)
}

// redisCorrect uses pipelined GETs, pure Go correction logic, and pipelined SETs.
// Two round trips to Redis, all correction logic in Go.
func (m *Manager) redisCorrect(ctx context.Context, streamKey string, parsed rewriter.Playlist) (*CorrectionResult, error) {
	// --- Read current state (single pipeline round trip) ---
	readPipe := m.rdb.Pipeline()
	msnCmd := readPipe.Get(ctx, prefixMSN+streamKey)
	dsnCmd := readPipe.Get(ctx, prefixDSN+streamKey)
	segCmd := readPipe.Get(ctx, prefixSeg+streamKey)
	offCmd := readPipe.Get(ctx, prefixOffset+streamKey)
	lastSegCmd := readPipe.Get(ctx, prefixLastSeg+streamKey)
	_, err := readPipe.Exec(ctx)

	// redis.Nil is expected for new stream keys — not an error
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis read pipeline: %w", err)
	}

	// Build prior state from Redis (defaults for missing keys)
	prior := rewriter.StreamState{
		LastMSN:        int64OrDefault(msnCmd, -1),
		LastDSN:        int64OrDefault(dsnCmd, -1),
		SegmentCount:   intOrDefault(segCmd, 0),
		Offset:         int64OrDefault(offCmd, 0),
		LastSegmentURI: stringOrDefault(lastSegCmd, ""),
	}

	// --- Pure Go correction (same logic as local path) ---
	corr, newState := rewriter.Correct(parsed, prior)

	// --- Write updated state (single pipeline round trip) ---
	ttl := m.cfg.StateTTL
	writePipe := m.rdb.Pipeline()
	writePipe.SetEx(ctx, prefixMSN+streamKey, newState.LastMSN, ttl)
	writePipe.SetEx(ctx, prefixDSN+streamKey, newState.LastDSN, ttl)
	writePipe.SetEx(ctx, prefixSeg+streamKey, newState.SegmentCount, ttl)
	writePipe.SetEx(ctx, prefixOffset+streamKey, newState.Offset, ttl)
	writePipe.SetEx(ctx, prefixLastSeg+streamKey, newState.LastSegmentURI, ttl)
	if _, err := writePipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis write pipeline: %w", err)
	}

	return &CorrectionResult{
		Correction: corr,
		NewState:   newState,
		Source:     "redis",
	}, nil
}

// localCorrect uses the in-memory state. This is safe per-instance: the state
// was either populated from Redis earlier, or this is the first request since
// instance boot.
func (m *Manager) localCorrect(streamKey string, parsed rewriter.Playlist) (*CorrectionResult, error) {
	val, exists := m.local.Load(streamKey)

	if !exists {
		// No prior state anywhere. If Redis is configured but down, this means
		// we can't verify correctness → fail closed.
		if m.rdb != nil {
			return nil, ErrNoState
		}

		// Local-only mode (no Redis configured): establish baseline
		state := &rewriter.StreamState{
			LastMSN:        parsed.MSN,
			LastDSN:        parsed.DSN,
			SegmentCount:   parsed.SegmentCount,
			Offset:         0,
			LastSegmentURI: parsed.LastSegmentURI,
		}
		m.local.Store(streamKey, state)

		return &CorrectionResult{
			Correction: rewriter.Correction{
				OriginalMSN:  parsed.MSN,
				CorrectedMSN: parsed.MSN,
				OriginalDSN:  parsed.DSN,
				CorrectedDSN: parsed.DSN,
			},
			NewState: *state,
			Source:   "baseline",
		}, nil
	}

	prior := val.(*rewriter.StreamState)
	corr, newState := rewriter.Correct(parsed, *prior)
	m.local.Store(streamKey, &newState)

	return &CorrectionResult{
		Correction: corr,
		NewState:   newState,
		Source:     "local",
	}, nil
}

// GetState returns current state for a stream (for stats/debug).
func (m *Manager) GetState(ctx context.Context, streamKey string) (*rewriter.StreamState, string, error) {
	// Try local first (fast)
	if val, ok := m.local.Load(streamKey); ok {
		return val.(*rewriter.StreamState), "local", nil
	}

	// Try Redis
	if m.rdb != nil {
		pipe := m.rdb.Pipeline()
		msnCmd := pipe.Get(ctx, prefixMSN+streamKey)
		dsnCmd := pipe.Get(ctx, prefixDSN+streamKey)
		segCmd := pipe.Get(ctx, prefixSeg+streamKey)
		offCmd := pipe.Get(ctx, prefixOffset+streamKey)
		lastSegCmd := pipe.Get(ctx, prefixLastSeg+streamKey)
		_, err := pipe.Exec(ctx)
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, "", fmt.Errorf("redis pipeline: %w", err)
		}

		msn := int64OrDefault(msnCmd, 0)
		dsn := int64OrDefault(dsnCmd, 0)

		if msn != 0 || dsn != 0 {
			state := &rewriter.StreamState{
				LastMSN:        msn,
				LastDSN:        dsn,
				SegmentCount:   intOrDefault(segCmd, 0),
				Offset:         int64OrDefault(offCmd, 0),
				LastSegmentURI: stringOrDefault(lastSegCmd, ""),
			}
			return state, "redis", nil
		}
	}

	return nil, "", ErrNoState
}

// RedisHealthy checks Redis connectivity. Returns false if Redis is not
// configured or unreachable.
func (m *Manager) RedisHealthy(ctx context.Context) bool {
	if m.rdb == nil {
		return false
	}
	return m.rdb.Ping(ctx).Err() == nil
}

// StreamCount returns approximate number of tracked streams in local cache.
func (m *Manager) StreamCount() int {
	count := 0
	m.local.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// Reset clears state for a stream (or all streams if key is empty).
// Uses SCAN + DEL instead of FLUSHDB to avoid nuking unrelated data
// if the Redis instance is shared.
func (m *Manager) Reset(ctx context.Context, streamKey string) error {
	if streamKey != "" {
		// Delete a specific stream's keys
		m.local.Delete(streamKey)
		if m.rdb != nil {
			pipe := m.rdb.Pipeline()
			for _, prefix := range allPrefixes {
				pipe.Del(ctx, prefix+streamKey)
			}
			_, err := pipe.Exec(ctx)
			return err
		}
		return nil
	}

	// Reset all — clear local cache
	m.local.Range(func(key, _ any) bool {
		m.local.Delete(key)
		return true
	})

	// SCAN + DEL for all our prefixes (safe for shared Redis)
	if m.rdb != nil {
		for _, prefix := range allPrefixes {
			if err := m.scanAndDelete(ctx, prefix+"*"); err != nil {
				return fmt.Errorf("scan+delete for prefix %q: %w", prefix, err)
			}
		}
	}
	return nil
}

// scanAndDelete removes all keys matching a pattern using SCAN (non-blocking).
func (m *Manager) scanAndDelete(ctx context.Context, pattern string) error {
	var cursor uint64
	for {
		keys, nextCursor, err := m.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := m.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// Close cleanly shuts down Redis connections.
func (m *Manager) Close() error {
	if m.rdb != nil {
		return m.rdb.Close()
	}
	return nil
}

// --- Redis value helpers ---

func int64OrDefault(cmd *redis.StringCmd, def int64) int64 {
	v, err := cmd.Int64()
	if err != nil {
		return def
	}
	return v
}

func intOrDefault(cmd *redis.StringCmd, def int) int {
	v, err := cmd.Int()
	if err != nil {
		return def
	}
	return v
}

func stringOrDefault(cmd *redis.StringCmd, def string) string {
	v, err := cmd.Result()
	if err != nil {
		return def
	}
	return v
}
