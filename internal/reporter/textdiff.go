package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/yamlview"
)

// Bounds of a text line diff. Longer texts keep the before/after
// pair; JSON always holds both full values.
const (
	textDiffMaxLines   = 2000
	textDiffMaxCells   = 1000000
	textDiffContext    = 2
	textDiffShownLines = 40
)

type textDiff struct {
	// object is "added" or "removed" when lines show a whole added or
	// removed object instead of a line diff of a replaced string.
	object                  string
	beforeLines, afterLines int
	added, removed          int
	// lines are hunk headers ("@@ -12 +12 @@") and lines marked with a space
	// (context), "+" (head only) or "-" (base only), in display order.
	lines []string
	// more counts changed lines past textDiffShownLines.
	more int
}

// textLineDiff compares a replaced multi-line string line by line, so a
// reviewer sees the lines a pull request adds and removes instead of two
// escaped values cut at the same place. Redacted values, values
// that are not both strings, single-line strings and texts over the bounds
// return false.
func textLineDiff(change domain.Change) (textDiff, bool) {
	before, bok := change.Before.(string)
	after, aok := change.After.(string)
	if change.Operation != domain.OperationReplace || change.Redacted || !bok || !aok || !(strings.Contains(before, "\n") || strings.Contains(after, "\n")) {
		return textDiff{}, false
	}
	b, a := textLines(before), textLines(after)
	if len(b) > textDiffMaxLines || len(a) > textDiffMaxLines {
		return textDiff{}, false
	}
	ops, ok := domain.LineEdits(b, a, textDiffMaxCells)
	if !ok {
		return textDiff{}, false
	}
	d := textDiff{beforeLines: len(b), afterLines: len(a)}
	show := make([]bool, len(ops))
	for i, op := range ops {
		if op.Mark == ' ' {
			continue
		}
		if op.Mark == '+' {
			d.added++
		} else {
			d.removed++
		}
		for j := max(0, i-textDiffContext); j <= min(len(ops)-1, i+textDiffContext); j++ {
			show[j] = true
		}
	}
	if d.added+d.removed == 0 {
		// Only a trailing newline differs; the pair shows that better.
		return textDiff{}, false
	}
	shown := 0
	for i, op := range ops {
		if !show[i] {
			continue
		}
		if op.Mark != ' ' {
			if shown == textDiffShownLines {
				d.more = d.added + d.removed - shown
				break
			}
			shown++
		}
		if i == 0 || !show[i-1] {
			d.lines = append(d.lines, fmt.Sprintf("@@ -%d +%d @@", op.Base, op.Head))
		}
		d.lines = append(d.lines, string(op.Mark)+op.Text)
	}
	return d, true
}

// textLines splits text into lines; a final newline does not start a line.
func textLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// summary describes the lines, such as "text changed (3 → 4 lines; 1
// added, 0 removed)" or "added (12 lines)".
func (d textDiff) summary(arrow string) string {
	if d.object != "" {
		return fmt.Sprintf("%s (%d lines)", d.object, d.beforeLines+d.afterLines)
	}
	return fmt.Sprintf("text changed (%d %s %d lines; %d added, %d removed)", d.beforeLines, arrow, d.afterLines, d.added, d.removed)
}

// moreText counts the lines past the shown bound.
func (d textDiff) moreText() string {
	if d.object != "" {
		return fmt.Sprintf("%d more line(s)", d.more)
	}
	return fmt.Sprintf("%d more changed line(s)", d.more)
}

// lineView shows a change as lines when that is easier to read than a
// before/after pair: a replaced multi-line string as a line diff, or an
// added or removed object as its YAML lines.
func lineView(change domain.Change) (textDiff, bool) {
	if d, ok := textLineDiff(change); ok {
		return d, true
	}
	return objectLines(change)
}

// objectBlockWidth bounds the one-line JSON of an added or removed object or
// list. A longer one, or one that holds a multi-line string, is shown as YAML
// lines.
const objectBlockWidth = 400

// objectLines shows an added or removed object or list, other than a whole
// resource, as YAML lines marked "+" or "-". A one-line JSON value
// past the display width would be cut, and a multi-line string inside it
// would show as one escaped line.
func objectLines(change domain.Change) (textDiff, bool) {
	value, mark, object := change.After, "+", "added"
	if change.Operation == domain.OperationRemove {
		value, mark, object = change.Before, "-", "removed"
	}
	if (change.Operation != domain.OperationAdd && change.Operation != domain.OperationRemove) || change.Path == "/" || change.Redacted {
		return textDiff{}, false
	}
	switch value.(type) {
	case map[string]any, []any:
	default:
		return textDiff{}, false
	}
	data := displayJSON(value)
	if data == nil || (len(data) <= objectBlockWidth && !multiLine(value)) {
		return textDiff{}, false
	}
	text, ok := yamlview.Text(value)
	if !ok {
		return textDiff{}, false
	}
	lines := textLines(text)
	d := textDiff{object: object}
	if mark == "+" {
		d.afterLines, d.added = len(lines), len(lines)
	} else {
		d.beforeLines, d.removed = len(lines), len(lines)
	}
	shown, more := bounded(lines, textDiffShownLines)
	for _, line := range shown {
		d.lines = append(d.lines, mark+line)
	}
	d.more = more
	return d, true
}

func multiLine(value any) bool {
	switch x := value.(type) {
	case string:
		return strings.Contains(x, "\n")
	case map[string]any:
		for _, v := range x {
			if multiLine(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if multiLine(v) {
				return true
			}
		}
	}
	return false
}
