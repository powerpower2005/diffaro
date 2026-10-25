package domain

import (
	"strings"
	"testing"
)

// A pair at exactly width runes is long only for views that bound at width
// runes or more; runes, not bytes, are counted.
func TestLongPairThresholds(t *testing.T) {
	atWidth, shorter := strings.Repeat("é", 4), "abc"
	if RunesOver(atWidth, shorter, 4) || RunesOver(shorter, atWidth, 4) {
		t.Fatal("RunesOver counts a side of exactly width runes as long")
	}
	if !RunesAtLeast(atWidth, shorter, 4) || !RunesAtLeast(shorter, atWidth, 4) {
		t.Fatal("RunesAtLeast does not count a side of exactly width runes as long")
	}
	if !RunesOver(atWidth+"x", shorter, 4) || RunesAtLeast(shorter, shorter, 4) {
		t.Fatal("thresholds misjudge sides past or under width")
	}
}

// A long pair starts lead runes before its first difference on both sides; a
// pair that is not long is unchanged.
func TestPairFromStartsNearTheFirstDifference(t *testing.T) {
	before, after := "ééééééééXY", "ééééééééZY"
	if b, a := PairFrom(before, after, false, 2); b != before || a != after {
		t.Fatalf("pair that is not long changed: %q %q", b, a)
	}
	if got := PairStart(before, after, true, 2); got != 6 {
		t.Fatalf("PairStart = %d, want 6 (first difference 8 minus lead 2)", got)
	}
	if b, a := PairFrom(before, after, true, 2); b != "…ééXY" || a != "…ééZY" {
		t.Fatalf("long pair = %q %q", b, a)
	}
	if b, a := PairFrom("aX", "aY", true, 5); b != "aX" || a != "aY" {
		t.Fatalf("difference within lead must not be cut: %q %q", b, a)
	}
}

// StartLongPairs shifts only a removed line directly followed by an added
// line, and only when the caller's threshold calls the pair long; the input
// lines stay as they were.
func TestStartLongPairsUsesTheCallerThreshold(t *testing.T) {
	before, after := strings.Repeat("a", 7)+"b", strings.Repeat("a", 7)+"c"
	lines := []string{"-" + before, "+" + after, " context", "-" + before}
	if got := StartLongPairs(lines, 8, RunesOver); strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("a pair of exactly width runes was shifted under RunesOver: %q", got)
	}
	got := StartLongPairs(lines, 8, RunesAtLeast)
	if got[0] != "-…aab" || got[1] != "+…aac" || got[2] != lines[2] || got[3] != lines[3] {
		t.Fatalf("pair not started near its difference, or other lines changed: %q", got)
	}
	if lines[0] != "-"+before {
		t.Fatal("input lines were modified")
	}
}
