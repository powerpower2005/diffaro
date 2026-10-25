// Package reporter encodes a domain.Report as full Markdown, compact PR
// Markdown, terminal text or versioned JSON (Encode). All formats come from
// the same report and are redacted and bounded. Output ordering is stable.
//
// Encode first redacts the report (Redact, which callers writing several
// formats run once and encode from), then builds one reportView with the facts
// the human-readable formats share; each encoder writes its sections from
// that view, and viewChange decides how every format shows a field change.
//
// WriteDiagnostics and WriteIssue (diagnostics.go) write the stderr
// diagnostics of a report or one issue; the CLI chooses the writer, its byte
// budget and the verbosity.
package reporter

import "diffaro/internal/domain"

// changeKind is how a field change reads best; every format renders the kind
// viewChange chooses.
type changeKind int

const (
	// changePair shows the before and after values.
	changePair changeKind = iota
	// changeHidden: redaction hides the value on both sides.
	changeHidden
	// changeLines shows a replaced multi-line string as a line diff
	// or an added or removed object as YAML lines.
	changeLines
	// changeList shows a whole-list replacement as its added and removed
	// items.
	changeList
	// changeWhole is a whole added or removed resource (wholeResource). PR
	// Markdown shows its value as lines; full Markdown and the terminal show
	// the familiar settings of a resource manifest and otherwise read it as
	// a pair, as they do a whole Application spec or input.
	changeWhole
)

type changeView struct {
	kind  changeKind
	lines textDiff  // changeLines
	items listItems // changeList
	whole wholeView // changeWhole
}

// changeOptions name the views a format uses besides the pair and the hidden
// value.
type changeOptions struct {
	// objectLines shows an added or removed object as YAML lines; without
	// it only replaced multi-line strings are changeLines.
	objectLines bool
	// lists shows a list whose before/after pair is longer than listWidth
	// runes as changeList.
	lists     bool
	listWidth int
}

// viewChange decides how a change reads: hidden, as lines, as list items or
// as a before/after pair.
func viewChange(change domain.Change, opt changeOptions) changeView {
	if wholeResource(change) {
		return changeView{kind: changeWhole, whole: newWholeView(change)}
	}
	if change.Hidden() {
		return changeView{kind: changeHidden}
	}
	view := textLineDiff
	if opt.objectLines {
		view = lineView
	}
	if lines, ok := view(change); ok {
		return changeView{kind: changeLines, lines: lines}
	}
	if opt.lists {
		if items, ok := listItemDiff(change, opt.listWidth); ok {
			return changeView{kind: changeList, items: items}
		}
	}
	return changeView{kind: changePair}
}

type listItems struct {
	added, removed []any
	reordered      string
}

// listItemGroup is the removed or the added items of a whole-list change,
// with the mark and name formats show them by.
type listItemGroup struct {
	mark, name string
	items      []any
}

// groups returns the removed items, then the added items: every format lists
// base items before head items, as a diff does.
func (l listItems) groups() []listItemGroup {
	return []listItemGroup{{"-", "removed", l.removed}, {"+", "added", l.added}}
}

// listItemDiff summarizes a whole-list replacement by item when the
// before/after pair would be longer than width, so a reviewer sees the added
// and removed entries of a list without item keys. JSON keeps the
// full before/after values.
func listItemDiff(change domain.Change, width int) (listItems, bool) {
	before, bok := change.Before.([]any)
	after, aok := change.After.([]any)
	if change.Operation != domain.OperationReplace || change.Redacted || !bok || !aok {
		return listItems{}, false
	}
	key := func(v any) string {
		b := displayJSON(v)
		return string(b)
	}
	if len([]rune(key(before))) <= width && len([]rune(key(after))) <= width {
		return listItems{}, false
	}
	remaining := map[string]int{}
	for _, item := range before {
		remaining[key(item)]++
	}
	out := listItems{}
	for _, item := range after {
		if k := key(item); remaining[k] > 0 {
			remaining[k]--
		} else {
			out.added = append(out.added, item)
		}
	}
	// Unmatched counts left in remaining are exactly the removed items.
	for _, item := range before {
		if k := key(item); remaining[k] > 0 {
			remaining[k]--
			out.removed = append(out.removed, item)
		}
	}
	if len(out.added) == 0 && len(out.removed) == 0 {
		out.reordered = "; order changed"
	}
	return out, true
}
