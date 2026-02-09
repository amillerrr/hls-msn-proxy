// Package rewriter provides HLS playlist parsing and MSN correction.
//
// This package is intentionally pure: no I/O, no Redis, no HTTP. It takes
// a playlist body and state, and returns a corrected playlist. All error
// paths are explicit.
//
// Change detection uses last segment URI comparison (works on all HLS versions,
// including V3 which lacks EXT-X-PROGRAM-DATE-TIME). Segment count is retained
// as a secondary signal but is not the primary differentiator.
package rewriter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxReasonableOffset is the threshold above which the offset is considered
// excessive — indicating sustained origin divergence or a bug. The proxy
// will still apply the offset (to avoid regressions) but will flag it for
// alerting. At 6s target duration, 10000 offset ≈ 16 hours of drift.
const MaxReasonableOffset int64 = 10000

var (
	msnRegex = regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)`)
	dsnRegex = regexp.MustCompile(`#EXT-X-DISCONTINUITY-SEQUENCE:(\d+)`)
)

// Playlist holds the parsed metadata from an HLS media playlist.
type Playlist struct {
	MSN            int64
	DSN            int64
	SegmentCount   int
	LastSegmentURI string // last segment URI in the playlist (version-agnostic change detector)
	IsMedia        bool   // true if media playlist (has #EXTINF or #EXT-X-TARGETDURATION)
	IsVOD          bool   // true if has #EXT-X-ENDLIST
}

// StreamState holds the last known good state for a stream.
type StreamState struct {
	LastMSN        int64
	LastDSN        int64
	SegmentCount   int
	Offset         int64
	LastSegmentURI string
}

// Correction describes what was changed and why.
type Correction struct {
	OriginalMSN     int64
	CorrectedMSN    int64
	OriginalDSN     int64
	CorrectedDSN    int64
	OffsetApplied   int64
	WasRegression   bool
	OffsetExcessive bool // true if offset exceeds MaxReasonableOffset
}

// Parse extracts MSN, DSN, segment count, and last segment URI from an HLS
// media playlist body. Works on all HLS versions (V3+).
func Parse(body []byte) Playlist {
	s := string(body)
	p := Playlist{}

	if strings.Contains(s, "#EXTINF:") || strings.Contains(s, "#EXT-X-TARGETDURATION:") {
		p.IsMedia = true
	}
	if strings.Contains(s, "#EXT-X-ENDLIST") {
		p.IsVOD = true
	}

	if m := msnRegex.FindStringSubmatch(s); len(m) == 2 {
		p.MSN, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := dsnRegex.FindStringSubmatch(s); len(m) == 2 {
		p.DSN, _ = strconv.ParseInt(m[1], 10, 64)
	}

	// Count #EXTINF occurrences (fixed string match, no regex needed)
	p.SegmentCount = strings.Count(s, "#EXTINF:")

	// Extract last segment URI: walk backward from end of playlist to find the
	// last non-empty, non-comment line. This is the URI of the final segment in
	// the sliding window — it changes whenever a new segment arrives.
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p.LastSegmentURI = line
		break
	}

	return p
}

// Correct computes the corrected MSN/DSN given upstream playlist metadata and
// prior state. Returns the correction details and updated state.
//
// Rules:
//   - corrected_msn = upstream_msn + offset
//   - If corrected_msn < last_msn → regression detected:
//     If last segment URI changed: target = last_msn + 1 (new content arrived)
//     If last segment URI same:    target = last_msn     (same content, different numbering)
//     If no prior URI to compare:  target = last_msn + 1 (conservative — prefer advancing)
//     Offset is adjusted so corrected_msn = target
//   - DSN never decreases (clamped to max of upstream and last seen)
//   - Offset is flagged as excessive if it exceeds MaxReasonableOffset
func Correct(parsed Playlist, prior StreamState) (Correction, StreamState) {
	upstreamMSN := parsed.MSN
	upstreamDSN := parsed.DSN

	offset := prior.Offset
	correctedMSN := upstreamMSN + offset
	wasRegression := false

	if prior.LastMSN >= 0 && correctedMSN < prior.LastMSN {
		wasRegression = true

		var targetMSN int64
		if prior.LastSegmentURI != "" && parsed.LastSegmentURI == prior.LastSegmentURI {
			// Exact same tail segment — same content, just renumbered.
			// Hold at last MSN to avoid false advancement.
			targetMSN = prior.LastMSN
		} else {
			// Different tail segment (or no prior URI to compare) — new content
			// arrived. Advance by 1. This is the conservative default: advancing
			// when content didn't change causes a harmless refetch; holding when
			// content did change causes a playback stall.
			targetMSN = prior.LastMSN + 1
		}

		offset = targetMSN - upstreamMSN
		correctedMSN = targetMSN
	}

	// DSN: never decrease
	correctedDSN := upstreamDSN
	if prior.LastDSN >= 0 && correctedDSN < prior.LastDSN {
		correctedDSN = prior.LastDSN
	}

	newState := StreamState{
		LastMSN:        correctedMSN,
		LastDSN:        correctedDSN,
		SegmentCount:   parsed.SegmentCount,
		Offset:         offset,
		LastSegmentURI: parsed.LastSegmentURI,
	}

	correction := Correction{
		OriginalMSN:     upstreamMSN,
		CorrectedMSN:    correctedMSN,
		OriginalDSN:     upstreamDSN,
		CorrectedDSN:    correctedDSN,
		OffsetApplied:   offset,
		WasRegression:   wasRegression,
		OffsetExcessive: offset > MaxReasonableOffset || offset < -MaxReasonableOffset,
	}

	return correction, newState
}

// Apply rewrites the MSN and DSN tags in the playlist body.
func Apply(body []byte, corr Correction) []byte {
	s := string(body)

	if corr.CorrectedMSN != corr.OriginalMSN {
		s = msnRegex.ReplaceAllString(s,
			fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", corr.CorrectedMSN))
	}

	if corr.CorrectedDSN != corr.OriginalDSN {
		s = dsnRegex.ReplaceAllString(s,
			fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d", corr.CorrectedDSN))
	}

	return []byte(s)
}
