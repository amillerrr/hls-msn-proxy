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
			expected: Playlist{MSN: 1042, DSN: 5, SegmentCount: 3, IsMedia: true, IsVOD: false},
		},
		{
			name: "vod playlist",
			body: `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg0.ts
#EXT-X-ENDLIST`,
			expected: Playlist{MSN: 0, DSN: 0, SegmentCount: 1, IsMedia: true, IsVOD: true},
		},
		{
			name: "master playlist",
			body: `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720
720p/playlist.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080p/playlist.m3u8`,
			expected: Playlist{MSN: 0, DSN: 0, SegmentCount: 0, IsMedia: false, IsVOD: false},
		},
		{
			name: "no dsn tag",
			body: `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:500
#EXTINF:6.000,
seg500.ts`,
			expected: Playlist{MSN: 500, DSN: 0, SegmentCount: 1, IsMedia: true, IsVOD: false},
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
	parsed := Playlist{MSN: 100, DSN: 3, SegmentCount: 3, IsMedia: true}
	prior := StreamState{LastMSN: 99, LastDSN: 3, SegmentCount: 3, Offset: 0}

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
}

func TestCorrect_RegressionSameSegments(t *testing.T) {
	// Upstream resets from 100 to 5 but segment count stays the same
	// → hold at last MSN (same content, just renumbered)
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 3, IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 2, SegmentCount: 3, Offset: 0}

	corr, newState := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 100 {
		t.Errorf("corrected MSN = %d, want 100 (hold)", corr.CorrectedMSN)
	}
	if newState.Offset != 95 {
		t.Errorf("offset = %d, want 95", newState.Offset)
	}
}

func TestCorrect_RegressionNewSegment(t *testing.T) {
	// Upstream resets from 100 to 5 AND segment count changes
	// → increment to last_msn + 1
	parsed := Playlist{MSN: 5, DSN: 0, SegmentCount: 4, IsMedia: true}
	prior := StreamState{LastMSN: 100, LastDSN: 2, SegmentCount: 3, Offset: 0}

	corr, newState := Correct(parsed, prior)

	if !corr.WasRegression {
		t.Error("should detect regression")
	}
	if corr.CorrectedMSN != 101 {
		t.Errorf("corrected MSN = %d, want 101 (increment)", corr.CorrectedMSN)
	}
	if newState.Offset != 96 {
		t.Errorf("offset = %d, want 96", newState.Offset)
	}
}

func TestCorrect_DSNNeverDecreases(t *testing.T) {
	parsed := Playlist{MSN: 100, DSN: 1, SegmentCount: 3, IsMedia: true}
	prior := StreamState{LastMSN: 99, LastDSN: 5, SegmentCount: 3, Offset: 0}

	corr, _ := Correct(parsed, prior)

	if corr.CorrectedDSN != 5 {
		t.Errorf("corrected DSN = %d, want 5 (clamped to prior)", corr.CorrectedDSN)
	}
}

func TestCorrect_ExistingOffset(t *testing.T) {
	// Already have an offset of 50 from a prior regression
	parsed := Playlist{MSN: 10, DSN: 0, SegmentCount: 4, IsMedia: true}
	prior := StreamState{LastMSN: 59, LastDSN: 0, SegmentCount: 3, Offset: 50}

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
	// First regression
	parsed1 := Playlist{MSN: 5, DSN: 0, SegmentCount: 4, IsMedia: true}
	prior1 := StreamState{LastMSN: 100, LastDSN: 0, SegmentCount: 3, Offset: 0}

	_, state1 := Correct(parsed1, prior1)
	// state1: LastMSN=101, Offset=96

	// Second regression (upstream resets again)
	parsed2 := Playlist{MSN: 2, DSN: 0, SegmentCount: 5, IsMedia: true}

	corr2, state2 := Correct(parsed2, state1)

	if !corr2.WasRegression {
		t.Error("should detect second regression")
	}
	// 2 + 96 = 98, which < 101, so regression. New segment → target = 102
	if corr2.CorrectedMSN != 102 {
		t.Errorf("corrected MSN = %d, want 102", corr2.CorrectedMSN)
	}
	if state2.Offset != 100 {
		t.Errorf("offset = %d, want 100", state2.Offset)
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

	if !contains(resultStr, "#EXT-X-MEDIA-SEQUENCE:105") {
		t.Error("MSN not rewritten to 105")
	}
	if !contains(resultStr, "#EXT-X-DISCONTINUITY-SEQUENCE:3") {
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

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
