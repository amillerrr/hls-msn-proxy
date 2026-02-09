package rewriter

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected Playlist
	}{
		{
			name: "standard media playlist",
			body: `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:1042
#EXT-X-DISCONTINUITY-SEQUENCE:5
#EXTINF:6.000,
seg1042.ts
#EXTINF:6.000,
seg1043.ts
#EXTINF:6.000,
seg1044.ts`,
			expected: Playlist{
				MSN: 1042, DSN: 5, SegmentCount: 3,
				LastSegmentURI: "seg1044.ts",
				IsMedia:        true, IsVOD: false,
			},
		},
		{
			name: "vod playlist",
			body: `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg0.ts
#EXT-X-ENDLIST`,
			expected: Playlist{
				MSN: 0, DSN: 0, SegmentCount: 1,
				LastSegmentURI: "seg0.ts",
				IsMedia:        true, IsVOD: true,
			},
		},
		{
			name: "master playlist",
			body: `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720
720p/playlist.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080p/playlist.m3u8`,
			expected: Playlist{
				MSN: 0, DSN: 0, SegmentCount: 0,
				LastSegmentURI: "1080p/playlist.m3u8",
				IsMedia:        false, IsVOD: false,
			},
		},
		{
			name: "no dsn tag",
			body: `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:500
#EXTINF:6.000,
seg500.ts`,
			expected: Playlist{
				MSN: 500, DSN: 0, SegmentCount: 1,
				LastSegmentURI: "seg500.ts",
				IsMedia:        true, IsVOD: false,
			},
		},
		{
			name: "playlist with trailing newlines",
			body: "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.000,\nseg10.ts\n\n\n",
			expected: Playlist{
				MSN: 10, DSN: 0, SegmentCount: 1,
				LastSegmentURI: "seg10.ts",
				IsMedia:        true, IsVOD: false,
			},
		},
		{
			name: "playlist with query parameters on segment URIs",
			body: `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:200
#EXTINF:6.000,
https://cdn.example.com/seg200.ts?token=abc123
#EXTINF:6.000,
https://cdn.example.com/seg201.ts?token=abc123`,
			expected: Playlist{
				MSN: 200, DSN: 0, SegmentCount: 2,
				LastSegmentURI: "https://cdn.example.com/seg201.ts?token=abc123",
				IsMedia:        true, IsVOD: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse([]byte(tt.body))
			if got != tt.expected {
				t.Errorf("got %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestCorrect_NoRegression(t *testing.T) {
	parsed := Playlist{MSN: 100, DSN: 3, SegmentCount: 3, LastSegmentURI: "seg102.ts", IsMedia: true}
	prior := StreamState{LastMSN: 99, LastDSN: 3, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg101.ts"}

	corr, newState := Correct(parsed, prior)

	if corr.WasRegression {
		t.Error("should not detect regression")
	}
	if corr.CorrectedMSN != 100 {
		t.Errorf("corrected MSN = %d, want 100", corr.CorrectedMSN)
	}
	if newState.LastMSN != 100 {
		t.Errorf("new state MSN = %d, want 100", newState.LastMSN)
	}
	if newState.Offset != 0 {
		t.Errorf("offset = %d, want 0", newState.Offset)
	}
	if newState.LastSegmentURI != "seg102.ts" {
		t.Errorf("last segment URI = %q, want %q", newState.LastSegmentURI, "seg102.ts")
	}
}

func TestCorrect_RegressionSameContent(t *testing.T) {
	// Upstream resets from 100 to 5 but last segment URI is the same
	// → same content, just renumbered → hold at last MSN
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg_common.ts", IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 2, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg_common.ts"}

	corr, newState := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 100 {
		t.Errorf("corrected MSN = %d, want 100 (hold — same content)", corr.CorrectedMSN)
	}
	if newState.Offset != 95 {
		t.Errorf("offset = %d, want 95", newState.Offset)
	}
}

func TestCorrect_RegressionNewContent(t *testing.T) {
	// Upstream resets from 100 to 5 AND last segment URI changed
	// → new content arrived → increment to last_msn + 1
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg_new.ts", IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 2, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg_old.ts"}

	corr, newState := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 101 {
		t.Errorf("corrected MSN = %d, want 101 (increment — new content)", corr.CorrectedMSN)
	}
	if newState.Offset != 96 {
		t.Errorf("offset = %d, want 96", newState.Offset)
	}
}

func TestCorrect_RegressionNoPriorURI(t *testing.T) {
	// Regression detected but no prior URI to compare (e.g., state was
	// established before LastSegmentURI tracking was added).
	// Conservative default: assume new content → increment.
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg5.ts", IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 0, SegmentCount: 3, Offset: 0, LastSegmentURI: ""}

	corr, _ := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 101 {
		t.Errorf("corrected MSN = %d, want 101 (conservative increment — no prior URI)", corr.CorrectedMSN)
	}
}

func TestCorrect_SlidingWindowSameCount(t *testing.T) {
	// Simulates the sliding window case that broke segment-count detection:
	// old segment dropped, new segment added, count stays the same,
	// but last segment URI changed → correctly detects new content.
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg_new_tail.ts", IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 0, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg_old_tail.ts"}

	corr, _ := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 101 {
		t.Errorf("corrected MSN = %d, want 101 (sliding window with new content)", corr.CorrectedMSN)
	}
}

func TestCorrect_DSNNeverDecreases(t *testing.T) {
	parsed := Playlist{MSN: 100, DSN: 1, SegmentCount: 3, LastSegmentURI: "seg.ts", IsMedia: true}
	prior := StreamState{LastMSN: 99, LastDSN: 5, SegmentCount: 3, Offset: 0, LastSegmentURI: "old.ts"}

	corr, _ := Correct(parsed, prior)

	if corr.CorrectedDSN != 5 {
		t.Errorf("corrected DSN = %d, want 5 (clamped to prior)", corr.CorrectedDSN)
	}
}

func TestCorrect_ExistingOffset(t *testing.T) {
	// Already have an offset of 50 from a prior regression
	parsed := Playlist{MSN: 10, DSN: 0, SegmentCount: 4, LastSegmentURI: "seg14.ts", IsMedia: true}
	prior := StreamState{LastMSN: 59, LastDSN: 0, SegmentCount: 3, Offset: 50, LastSegmentURI: "seg13.ts"}

	corr, newState := Correct(parsed, prior)

	// 10 + 50 = 60, which is > 59, so no regression
	if corr.WasRegression {
		t.Error("should not detect regression (10 + 50 = 60 > 59)")
	}
	if corr.CorrectedMSN != 60 {
		t.Errorf("corrected MSN = %d, want 60", corr.CorrectedMSN)
	}
	if newState.Offset != 50 {
		t.Errorf("offset should remain 50, got %d", newState.Offset)
	}
}

func TestCorrect_DoubleRegression(t *testing.T) {
	// First regression: different tail segment → increment
	parsed1 := Playlist{MSN: 5, DSN: 0, SegmentCount: 4, LastSegmentURI: "seg_b1.ts", IsMedia: true}
	prior1 := StreamState{LastMSN: 100, LastDSN: 0, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg_a1.ts"}

	_, state1 := Correct(parsed1, prior1)
	// state1: LastMSN=101, Offset=96

	// Second regression (upstream resets again, different content)
	parsed2 := Playlist{MSN: 2, DSN: 0, SegmentCount: 5, LastSegmentURI: "seg_c1.ts", IsMedia: true}

	corr2, state2 := Correct(parsed2, state1)

	if !corr2.WasRegression {
		t.Error("should detect second regression")
	}
	// 2 + 96 = 98, which < 101, so regression. New segment URI → target = 102
	if corr2.CorrectedMSN != 102 {
		t.Errorf("corrected MSN = %d, want 102", corr2.CorrectedMSN)
	}
	if state2.Offset != 100 {
		t.Errorf("offset = %d, want 100", state2.Offset)
	}
}

func TestCorrect_OffsetExcessive(t *testing.T) {
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg_new.ts", IsMedia: true}
	prior := StreamState{
		LastMSN:        MaxReasonableOffset + 100,
		LastDSN:        0,
		SegmentCount:   3,
		Offset:         0,
		LastSegmentURI: "seg_old.ts",
	}

	corr, _ := Correct(parsed, prior)

	if !corr.OffsetExcessive {
		t.Error("offset should be flagged as excessive")
	}
}

func TestCorrect_OffsetNotExcessive(t *testing.T) {
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, LastSegmentURI: "seg_new.ts", IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 0, SegmentCount: 3, Offset: 0, LastSegmentURI: "seg_old.ts"}

	corr, _ := Correct(parsed, prior)

	if corr.OffsetExcessive {
		t.Error("offset should not be flagged as excessive")
	}
}

func TestCorrect_FirstRequest(t *testing.T) {
	// First request for a stream — prior is zero-value with LastMSN = -1
	parsed := Playlist{MSN: 500, DSN: 3, SegmentCount: 5, LastSegmentURI: "seg504.ts", IsMedia: true}
	prior := StreamState{LastMSN: -1, LastDSN: -1}

	corr, newState := Correct(parsed, prior)

	if corr.WasRegression {
		t.Error("first request should not be a regression")
	}
	if corr.CorrectedMSN != 500 {
		t.Errorf("corrected MSN = %d, want 500", corr.CorrectedMSN)
	}
	if newState.LastMSN != 500 {
		t.Errorf("new state MSN = %d, want 500", newState.LastMSN)
	}
}

func TestApply(t *testing.T) {
	body := []byte(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:5
#EXT-X-DISCONTINUITY-SEQUENCE:1
#EXTINF:6.000,
seg5.ts`)

	corr := Correction{
		OriginalMSN:  5,
		CorrectedMSN: 105,
		OriginalDSN:  1,
		CorrectedDSN: 3,
	}

	result := Apply(body, corr)
	resultStr := string(result)

	if !containsStr(resultStr, "#EXT-X-MEDIA-SEQUENCE:105") {
		t.Error("MSN not rewritten to 105")
	}
	if !containsStr(resultStr, "#EXT-X-DISCONTINUITY-SEQUENCE:3") {
		t.Error("DSN not rewritten to 3")
	}
}

func TestApply_NoChange(t *testing.T) {
	body := []byte(`#EXTM3U
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:6.000,
seg.ts`)

	corr := Correction{
		OriginalMSN:  100,
		CorrectedMSN: 100,
		OriginalDSN:  0,
		CorrectedDSN: 0,
	}

	result := Apply(body, corr)
	if string(result) != string(body) {
		t.Error("body should be unchanged when no correction needed")
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && strings.Contains(s, substr)
}
