package manifest

import (
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
	"diffaro/internal/yamlview"
)

// Bounds of a resource's line hunks. A resource past them has no
// hunks, and reports show its field changes instead.
const (
	hunkMaxTextLines = 2000
	hunkMaxCells     = 1000000
	hunkContext      = 8
	// A longer previous block shows its first hunkLeadLines lines and an
	// elision; its start is searched up to hunkBlockSearch lines back.
	hunkLeadLines   = 2
	hunkBlockSearch = 200
	hunkMaxChanged  = 120
	hunkMaxHunks    = 20
	hunkLineRunes   = 1000
)

// lineHunks diffs the YAML lines of two redacted objects. hidden
// lists the named paths of values that changed while redaction hides them on
// both sides. The steps are:
//
//  1. print both sides as YAML lines with the path of each line;
//  2. tag the masked lines of hidden changes so they cannot match;
//  3. diff the lines and turn the tags into a "value changed (hidden)" note;
//  4. move added or removed runs to start at a block's first line;
//  5. group each run of changed lines with one neighbouring block of
//     context on each side.
//
// It returns nil, so reports show field changes, when a side cannot be
// printed, a hidden change has no masked line, or a bound is passed.
func lineHunks(base, head any, hidden []string) []domain.LineHunk {
	b, ok := printSide(base, head, hidden, baseTag)
	if !ok {
		return nil
	}
	h, ok := printSide(head, base, hidden, headTag)
	if !ok {
		return nil
	}
	ops, ok := domain.LineEdits(b.lines, h.lines, hunkMaxCells)
	if !ok {
		return nil
	}
	untagHidden(ops)
	slideToBlockStarts(ops)
	hunks, changed := groupHunks(ops, func(op domain.LineEdit) []string {
		if op.Mark == '-' {
			return b.paths[op.Base-1]
		}
		return h.paths[op.Head-1]
	})
	if len(hunks) > hunkMaxHunks || changed > hunkMaxChanged {
		return nil
	}
	for i := range hunks {
		hunks[i].Lines = boundPairs(hunks[i].Lines, hunkLineRunes)
	}
	return hunks
}

// side is one revision's YAML lines and the path of each line.
type side struct {
	lines []string
	paths [][]string
}

// printSide prints a value as YAML lines, naming list items as change paths
// do from both value and other, and tags the masked lines of hidden
// changes. It reports false past the line bound or when a hidden
// path has no masked line.
func printSide(value, other any, hidden []string, tag string) (side, bool) {
	lines, paths, ok := yamlview.Lines(value, other)
	if !ok || len(lines) > hunkMaxTextLines {
		return side{}, false
	}
	joined := make([]string, len(paths))
	for i, p := range paths {
		joined[i] = strings.Join(p, "")
	}
	for _, h := range hidden {
		found := false
		for i, line := range lines {
			p := joined[i]
			under := p == h || strings.HasPrefix(p, h+"/") || strings.HasPrefix(p, h+"[")
			if under && strings.Contains(line, security.Mask) {
				if !strings.HasSuffix(line, tag) {
					lines[i] = line + tag
				}
				found = true
			}
		}
		if !found {
			return side{}, false
		}
	}
	return side{lines, paths}, true
}

// Tags keep the masked lines of a hidden change from matching between base
// and head, so they read as removed and added; untagHidden turns them into
// hiddenNote.
const (
	baseTag    = "\x00base"
	headTag    = "\x00head"
	hiddenNote = "  # " + domain.HiddenChange
)

func untagHidden(ops []domain.LineEdit) {
	for i := range ops {
		if text, tagged := strings.CutSuffix(ops[i].Text, baseTag); tagged {
			ops[i].Text = text + hiddenNote
		} else if text, tagged := strings.CutSuffix(ops[i].Text, headTag); tagged {
			ops[i].Text = text + hiddenNote
		}
	}
}

// groupHunks turns each run of changed lines into a hunk with its context,
// merging hunks whose contexts meet. It returns the hunks and the number of
// changed lines.
func groupHunks(ops []domain.LineEdit, path func(domain.LineEdit) []string) ([]domain.LineHunk, int) {
	var hunks []domain.LineHunk
	end, changed := -1, 0
	for s := 0; s < len(ops); {
		if ops[s].Mark == ' ' {
			s++
			continue
		}
		e := s
		for e < len(ops) && ops[e].Mark != ' ' {
			e++
		}
		changed += e - s
		indent := minIndent(ops[s:e])
		from, block := contextBefore(ops, s, indent)
		to, cut := contextAfter(ops, e, indent)
		after := markedLines(ops[e:to])
		if cut {
			after = append(after, elision(ops[to].Text))
		}
		lines := append(markedLines(ops[s:e]), after...)
		p := commonPrefix(ops[s:e], path)
		switch {
		case len(hunks) > 0 && from <= end:
			// Contexts meet: extend the previous hunk.
			last := &hunks[len(hunks)-1]
			last.Lines = append(append(last.Lines, markedLines(ops[end:s])...), lines...)
			last.Path = commonPath(last.Path, p)
		case len(hunks) > 0 && block >= 0 && block < end:
			// The previous hunk already names the block before this run.
			last := &hunks[len(hunks)-1]
			last.Lines = append(append(last.Lines, elision(ops[s].Text)), lines...)
			last.Path = commonPath(last.Path, p)
		default:
			first, before := from, markedLines(ops[from:s])
			if block >= 0 {
				first = block
				before = append(markedLines(ops[block:block+hunkLeadLines]), elision(ops[block+hunkLeadLines].Text))
			}
			hunks = append(hunks, domain.LineHunk{Path: p, BaseStart: ops[first].Base, HeadStart: ops[first].Head, Lines: append(before, lines...)})
		}
		end, s = to, e
	}
	return hunks, changed
}

// contextBefore returns where the context before a changed run at s
// starts: back to the previous line indented no deeper than the run, up to
// hunkContext lines. When that line is further back, the context is only
// the first hunkLeadLines lines of its block, which name it, and block is
// its index; otherwise block is -1.
func contextBefore(ops []domain.LineEdit, s, indent int) (from, block int) {
	from = s
	for n := 0; from > 0 && ops[from-1].Mark == ' ' && n < hunkContext; n++ {
		from--
		if lineIndent(ops[from].Text) <= indent {
			return from, -1
		}
	}
	if from != s-hunkContext {
		return from, -1
	}
	for i := from - 1; i >= 0 && from-i <= hunkBlockSearch && ops[i].Mark == ' '; i-- {
		if lineIndent(ops[i].Text) <= indent {
			return s, i
		}
	}
	return from, -1
}

// contextAfter returns where the context after a changed run ending at e
// ends: the lines nested deeper than the run, then the next line indented
// no deeper and the lines nested under it, up to hunkContext lines. cut
// reports that the bound left lines of that block out.
func contextAfter(ops []domain.LineEdit, e, indent int) (to int, cut bool) {
	to, n := e, 0
	deeper := func(level int) bool {
		for to < len(ops) && ops[to].Mark == ' ' && lineIndent(ops[to].Text) > level {
			if n == hunkContext {
				return true
			}
			to, n = to+1, n+1
		}
		return false
	}
	if deeper(indent) {
		return to, true
	}
	if to < len(ops) && ops[to].Mark == ' ' && n < hunkContext {
		start := lineIndent(ops[to].Text)
		to, n = to+1, n+1
		return to, deeper(start)
	}
	return to, false
}

func minIndent(ops []domain.LineEdit) int {
	indent := -1
	for _, op := range ops {
		if i := lineIndent(op.Text); indent < 0 || i < indent {
			indent = i
		}
	}
	return indent
}

// elision marks context lines left out, at the indent of the line after it.
func elision(next string) string {
	return " " + strings.Repeat(" ", lineIndent(next)) + "…"
}

func markedLines(ops []domain.LineEdit) []string {
	lines := make([]string, len(ops))
	for i, op := range ops {
		lines[i] = string(op.Mark) + op.Text
	}
	return lines
}

// commonPrefix returns the joined common path of changed lines.
func commonPrefix(ops []domain.LineEdit, path func(domain.LineEdit) []string) string {
	common := path(ops[0])
	for _, op := range ops[1:] {
		common = common[:prefixLength(common, path(op))]
	}
	return strings.Join(common, "")
}

// commonPath returns the longest common segment prefix of two joined paths.
func commonPath(a, b string) string {
	sa := splitPath(a)
	return strings.Join(sa[:prefixLength(sa, splitPath(b))], "")
}

func prefixLength(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// splitPath splits a joined path into "/key" and "[name]" segments.
func splitPath(p string) []string {
	var segments []string
	start := 0
	for i := 1; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' || p[i] == '[' {
			segments = append(segments, p[start:i])
			start = i
		}
	}
	return segments
}

// slideToBlockStarts moves each run of only added or only removed lines, as
// far as equal lines allow, so it starts at its least indented line, and
// renumbers the lines. An added list item then reads as one block instead of
// starting inside the item before it.
func slideToBlockStarts(ops []domain.LineEdit) {
	for s := 0; s < len(ops); {
		if ops[s].Mark == ' ' {
			s++
			continue
		}
		e := s
		for e < len(ops) && ops[e].Mark == ops[s].Mark {
			e++
		}
		if (s > 0 && ops[s-1].Mark != ' ') || (e < len(ops) && ops[e].Mark != ' ') {
			// A replacement keeps its alignment.
			s = e
			continue
		}
		mark := ops[s].Mark
		for s > 0 && ops[s-1].Mark == ' ' && ops[s-1].Text == ops[e-1].Text {
			ops[s-1].Mark, ops[e-1].Mark = mark, ' '
			s, e = s-1, e-1
		}
		best, bestIndent := s, lineIndent(ops[s].Text)
		for e < len(ops) && ops[e].Mark == ' ' && ops[e].Text == ops[s].Text {
			ops[s].Mark, ops[e].Mark = ' ', mark
			s, e = s+1, e+1
			if i := lineIndent(ops[s].Text); i < bestIndent {
				best, bestIndent = s, i
			}
		}
		for s > best {
			ops[s-1].Mark, ops[e-1].Mark = mark, ' '
			s, e = s-1, e-1
		}
		s = e
	}
	base, head := 1, 1
	for i := range ops {
		ops[i].Base, ops[i].Head = base, head
		if ops[i].Mark != '+' {
			base++
		}
		if ops[i].Mark != '-' {
			head++
		}
	}
}

func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// boundPairs cuts lines to width runes after their mark. A removed line
// followed by its added counterpart longer than width (domain.RunesOver, the
// pairs this cut shortens) first starts shortly before their first difference
// (domain.StartLongPairs), so a change past the bound stays visible.
func boundPairs(lines []string, width int) []string {
	out := domain.StartLongPairs(lines, width, domain.RunesOver)
	for i, line := range out {
		if r := []rune(line); len(r) > width+1 {
			out[i] = string(r[:width+1]) + "…"
		}
	}
	return out
}
