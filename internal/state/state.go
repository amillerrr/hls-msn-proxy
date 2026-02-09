// Package state manages MSN state with Redis as primary and an in-memory
// cache as hot read layer. All Redis mutations use a Lua script for atomicity,
// eliminating distributed locks entirely.
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

	"github.com/redis/go-redis/v9"
	"github.com/amillerrr/hls-msn-proxy/internal/rewriter"
)

var (
	// ErrNoState means we have no prior state for this stream and cannot verify
	// MSN correctness. On a healthy cluster this only happens on the very first
	// request for a new stream (which is fine — first request establishes baseline).
	// If Redis is down and local cache is cold, this signals "fail closed".
	ErrNoState = errors.New("no prior state available")
)

// atomicCorrectScript is a Redis Lua script that performs the entire
// read → compare → correct → write cycle in a single atomic operation.
// No distributed locks. No race conditions. One round trip.
//
// KEYS: [msn_key, dsn_key, seg_key, offset_key]
// ARGV: [upstream_msn, upstream_dsn, segment_count, ttl_seconds]
// Returns: [corrected_msn, corrected_dsn, offset, was_regression(0|1)]
var atomicCorrectScript = redis.NewScript(`
local msn_key    = KEYS[1]
local dsn_key    = KEYS[2]
local seg_key    = KEYS[3]
local offset_key = KEYS[4]

local upstream_msn   = tonumber(ARGV[1])
local upstream_dsn   = tonumber(ARGV[2])
local segment_count  = tonumber(ARGV[3])
local ttl            = tonumber(ARGV[4])

-- Read current state
local last_msn   = tonumber(redis.call('GET', msn_key)) or -1
local last_dsn   = tonumber(redis.call('GET', dsn_key)) or -1
local last_seg   = tonumber(redis.call('GET', seg_key)) or 0
local offset     = tonumber(redis.call('GET', offset_key)) or 0

local corrected_msn = upstream_msn + offset
local was_regression = 0

if last_msn >= 0 and corrected_msn < last_msn then
    was_regression = 1
    local target_msn
    if segment_count ~= last_seg then
        target_msn = last_msn + 1
    else
        target_msn = last_msn
    end
    offset = target_msn - upstream_msn
    corrected_msn = target_msn
end

local corrected_dsn = upstream_dsn
if corrected_dsn < last_dsn then
    corrected_dsn = last_dsn
end

-- Write updated state atomically
redis.call('SETEX', msn_key, ttl, corrected_msn)
redis.call('SETEX', dsn_key, ttl, corrected_dsn)
redis.call('SETEX', seg_key, ttl, segment_count)
redis.call('SETEX', offset_key, ttl, offset)

return {corrected_msn, corrected_dsn, offset, was_regression}
`)

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

// CorrectionResult is the outcome of an atomic correct-and-update.
type CorrectionResult struct {
	Correction rewriter.Correction
	NewState   rewriter.StreamState
	Source     string // "redis", "local", "baseline"
}

// CorrectAndUpdate atomically reads prior state, computes MSN correction,
// and writes updated state. Returns the correction to apply.
//
// Fail-closed behavior:
//   - Redis healthy → atomic Lua script (single round trip, no races)
//   - Redis down + local state exists → local correction (safe for this instance)
//   - Redis down + no local state → ErrNoState (caller must serve stale or 503)
//   - First-ever request for stream → establishes baseline (no prior state to violate)
func (m *Manager) CorrectAndUpdate(ctx context.Context, streamKey string, parsed rewriter.Playlist) (*CorrectionResult, error) {
	// Try Redis first (atomic, cross-instance consistent)
	if m.rdb != nil {
		result, err := m.redisAtomicCorrect(ctx, streamKey, parsed)
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

// redisAtomicCorrect uses the Lua script for a single atomic round trip.
func (m *Manager) redisAtomicCorrect(ctx context.Context, streamKey string, parsed rewriter.Playlist) (*CorrectionResult, error) {
	keys := []string{
		"msn:" + streamKey,
		"dsn:" + streamKey,
		"seg:" + streamKey,
		"off:" + streamKey,
	}

	ttlSeconds := int64(m.cfg.StateTTL.Seconds())

	result, err := atomicCorrectScript.Run(ctx, m.rdb, keys,
		parsed.MSN,
		parsed.DSN,
		parsed.SegmentCount,
		ttlSeconds,
	).Int64Slice()

	if err != nil {
		return nil, fmt.Errorf("redis eval: %w", err)
	}

	if len(result) != 4 {
		return nil, fmt.Errorf("unexpected redis result length: %d", len(result))
	}

	correctedMSN := result[0]
	correctedDSN := result[1]
	offset := result[2]
	wasRegression := result[3] == 1

	return &CorrectionResult{
		Correction: rewriter.Correction{
			OriginalMSN:   parsed.MSN,
			CorrectedMSN:  correctedMSN,
			OriginalDSN:   parsed.DSN,
			CorrectedDSN:  correctedDSN,
			OffsetApplied: offset,
			WasRegression: wasRegression,
		},
		NewState: rewriter.StreamState{
			LastMSN:      correctedMSN,
			LastDSN:      correctedDSN,
			SegmentCount: parsed.SegmentCount,
			Offset:       offset,
		},
		Source: "redis",
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
			LastMSN:      parsed.MSN,
			LastDSN:      parsed.DSN,
			SegmentCount: parsed.SegmentCount,
			Offset:       0,
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
		msnCmd := pipe.Get(ctx, "msn:"+streamKey)
		dsnCmd := pipe.Get(ctx, "dsn:"+streamKey)
		segCmd := pipe.Get(ctx, "seg:"+streamKey)
		offCmd := pipe.Get(ctx, "off:"+streamKey)
		_, err := pipe.Exec(ctx)
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, "", fmt.Errorf("redis pipeline: %w", err)
		}

		msn, _ := msnCmd.Int64()
		dsn, _ := dsnCmd.Int64()
		seg, _ := segCmd.Int()
		off, _ := offCmd.Int64()

		if msn != 0 || dsn != 0 {
			state := &rewriter.StreamState{
				LastMSN: msn, LastDSN: dsn,
				SegmentCount: seg, Offset: off,
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
func (m *Manager) Reset(ctx context.Context, streamKey string) error {
	if streamKey != "" {
		m.local.Delete(streamKey)
		if m.rdb != nil {
			pipe := m.rdb.Pipeline()
			for _, prefix := range []string{"msn:", "dsn:", "seg:", "off:"} {
				pipe.Del(ctx, prefix+streamKey)
			}
			_, err := pipe.Exec(ctx)
			return err
		}
		return nil
	}

	// Reset all
	m.local.Range(func(key, _ any) bool {
		m.local.Delete(key)
		return true
	})
	if m.rdb != nil {
		return m.rdb.FlushDB(ctx).Err()
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
