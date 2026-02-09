// Package rewriter provides HLS playlist parsing and MSN correction.
//
// This package is intentionally pure: no I/O, no Redis, no HTTP. It takes
// a playlist body and state, and returns a corrected playlist. All error
// paths are explicit.
package rewriter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	msnRegex = regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)`)
	dsnRegex = regexp.MustCompile(`#EXT-X-DISCONTINUITY-SEQUENCE:(\d+)`)
	infRegex = regexp.MustCompile(`#EXTINF:`)
)

// Playlist holds the parsed metadata from an HLS media playlist.
type Playlist struct {
	MSN          int64
	DSN          int64
	SegmentCount int
	IsMedia      bool // true if media playlist (has #EXTINF or #EXT-X-TARGETDURATION)
	IsVOD        bool // true if has #EXT-X-ENDLIST
}

// StreamState holds the last known good state for a stream.
type StreamState struct {
	LastMSN      int64
	LastDSN      int64
	SegmentCount int
	Offset       int64
}

// Correction describes what was changed and why.
type Correction struct {
	OriginalMSN   int64
	CorrectedMSN  int64
	OriginalDSN   int64
	CorrectedDSN  int64
	OffsetApplied int64
	WasRegression bool
}

// Parse extracts MSN, DSN, and segment count from an HLS playlist body.
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

	p.SegmentCount = len(infRegex.FindAllStringIndex(s, -1))

	return p
}

// Correct computes the corrected MSN/DSN given upstream playlist metadata and
// prior state. Returns the correction details and updated state.
//
// Rules:
//   - corrected_msn = upstream_msn + offset
//   - If corrected_msn < last_msn → regression detected:
//     If segment count changed: target = last_msn + 1 (new segment arrived)
//     If segment count same:    target = last_msn     (same content, different numbering)
//     Offset is adjusted so corrected_msn = target
//   - DSN never decreases (clamped to max of upstream and last seen)
func Correct(parsed Playlist, prior StreamState) (Correction, StreamState) {
	upstreamMSN := parsed.MSN
	upstreamDSN := parsed.DSN

	offset := prior.Offset
	correctedMSN := upstreamMSN + offset
	wasRegression := false

	if correctedMSN < prior.LastMSN {
		wasRegression = true

		var targetMSN int64
		if parsed.SegmentCount != prior.SegmentCount {
			// Segment count changed — a new segment arrived, increment
			targetMSN = prior.LastMSN + 1
		} else {
			// Same content, just renumbered — hold at last value
			targetMSN = prior.LastMSN
		}

		offset = targetMSN - upstreamMSN
		correctedMSN = targetMSN
	}

	correctedDSN := upstreamDSN
	if correctedDSN < prior.LastDSN {
		correctedDSN = prior.LastDSN
	}

	newState := StreamState{
		LastMSN:      correctedMSN,
		LastDSN:      correctedDSN,
		SegmentCount: parsed.SegmentCount,
		Offset:       offset,
	}

	correction := Correction{
		OriginalMSN:   upstreamMSN,
		CorrectedMSN:  correctedMSN,
		OriginalDSN:   upstreamDSN,
		CorrectedDSN:  correctedDSN,
		OffsetApplied: offset,
		WasRegression: wasRegression,
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
