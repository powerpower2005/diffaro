package domain

import (
	"strings"
	"unicode/utf8"
)

// NamedPath shows a change path with the name of each list item it passes
// through, such as /spec/containers[api]/image, when lists were compared by
// item name. Index segments receive names only when their count
// matches, so a numeric map key never receives a name.
func NamedPath(path string, names []string) string {
	if len(names) == 0 {
		return path
	}
	parts := strings.Split(path, "/")
	index := map[int]bool{}
	for i, part := range parts[1:] {
		if part != "" && strings.Trim(part, "0123456789") == "" {
			index[i+1] = true
		}
	}
	if len(index) != len(names) {
		return path
	}
	var b strings.Builder
	for i, part := range parts[1:] {
		if index[i+1] {
			b.WriteString("[" + names[0] + "]")
			names = names[1:]
		} else {
			b.WriteString("/" + part)
		}
	}
	return b.String()
}

// DiffStart returns the rune index shortly, context runes, before the first
// difference of two strings, where a bounded view of both should start.
func DiffStart(before, after string, context int) int {
	b, a := []rune(before), []rune(after)
	common := 0
	for common < len(b) && common < len(a) && b[common] == a[common] {
		common++
	}
	return max(0, common-context)
}

// FromRune drops the first start runes of s and marks the cut with "…".
func FromRune(s string, start int) string {
	if start == 0 {
		return s
	}
	return "…" + string([]rune(s)[start:])
}

// A before/after pair is long when either side is past the width its view
// bounds it at. Each view chooses its threshold: RunesOver (more than width
// runes), RunesAtLeast (width runes or more) or its own, such as a byte
// count.

// RunesOver reports whether either side has more than width runes.
func RunesOver(before, after string, width int) bool {
	return utf8.RuneCountInString(before) > width || utf8.RuneCountInString(after) > width
}

// RunesAtLeast reports whether either side has width runes or more.
func RunesAtLeast(before, after string, width int) bool {
	return utf8.RuneCountInString(before) >= width || utf8.RuneCountInString(after) >= width
}

// PairStart returns the rune both sides of a long pair start at: lead runes
// before their first difference (DiffStart), so values that differ only
// past a display bound do not look identical. A pair that is not long
// starts at 0.
func PairStart(before, after string, long bool, lead int) int {
	if !long {
		return 0
	}
	return DiffStart(before, after, lead)
}

// PairFrom starts both sides of a long pair at PairStart, marking the cut
// with "…". A pair that is not long is returned as given.
func PairFrom(before, after string, long bool, lead int) (string, string) {
	start := PairStart(before, after, long, lead)
	return FromRune(before, start), FromRune(after, start)
}

// StartLongPairs returns a copy of diff lines in which each removed line
// ("-") followed by an added line ("+") starts width/4 runes before their
// first difference (PairFrom) when long reports the pair, without marks,
// past width. Each caller passes the threshold of the bound it cuts lines
// at (RunesOver or RunesAtLeast), so a change past the bound stays visible.
func StartLongPairs(lines []string, width int, long func(before, after string, width int) bool) []string {
	out := append([]string(nil), lines...)
	for i := 0; i+1 < len(out); i++ {
		if !strings.HasPrefix(out[i], "-") || !strings.HasPrefix(out[i+1], "+") {
			continue
		}
		before, after := out[i][1:], out[i+1][1:]
		if !long(before, after, width) {
			continue
		}
		before, after = PairFrom(before, after, true, width/4)
		out[i], out[i+1] = "-"+before, "+"+after
	}
	return out
}
