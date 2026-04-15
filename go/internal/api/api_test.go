package api

import "testing"

func TestParseRangeSupports48h(t *testing.T) {
	const want = 48 * 60 * 60 * 1000
	if got := parseRange("48h"); got != want {
		t.Fatalf("parseRange(48h) = %d, want %d", got, want)
	}
}
