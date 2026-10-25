package reporter

import (
	"fmt"
	"strconv"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
	"diffaro/internal/yamlview"
)

// Diff block lines of PR Markdown. Each function turns
// one resource or change into lines marked " ", "-" or "+", with "@@" lines
// naming paths; writeDiffBlock prints them. Values are YAML (yamlview), as in
// line hunks.

// resourceLines returns the diff block lines of a changed resource: its
// line hunks when the comparison produced them, otherwise its field
// changes, up to prMaxFields.
func resourceLines(res domain.ResourceDiff) []string {
	if len(res.LineHunks) > 0 {
		return hunkLines(res.LineHunks)
	}
	lines := []string{}
	for i, change := range res.Changes {
		if i == prMaxFields {
			lines = append(lines, fmt.Sprintf(" … %d more field change(s) in JSON or the full Markdown report", len(res.Changes)-i))
			break
		}
		lines = append(lines, fieldDiffLines(change)...)
	}
	return lines
}

// hunkLines prints line hunks under "@@ -<base line> +<head line> @@ <path>".
// Hidden changes are already in them, noted where they are.
func hunkLines(hunks []domain.LineHunk) []string {
	lines := []string{}
	for _, h := range hunks {
		lines = append(lines, fmt.Sprintf("@@ -%d +%d @@ %s", h.BaseStart, h.HeadStart, h.Path))
		lines = append(lines, pairStarts(h.Lines)...)
	}
	return lines
}

// fieldDiffLines shows one change: a "@@ path @@" header, base lines marked
// "-" and head lines marked "+".
func fieldDiffLines(change domain.Change) []string {
	// An added or removed object reads as its value lines below, so only
	// replaced multi-line strings are line diffs here.
	view := viewChange(change, changeOptions{lists: true})
	switch view.kind {
	case changeWhole:
		return valueLines(view.whole.mark, view.whole.value, "")
	case changeHidden:
		return hiddenLines(change)
	case changeLines:
		return textChangeLines(change, view.lines)
	case changeList:
		return listChangeLines(change, view.items)
	}
	return valueChangeLines(change)
}

// hiddenLines shows a value hidden on both sides as its masked lines,
// removed and added, noted as hidden, as line hunks do.
func hiddenLines(change domain.Change) []string {
	lines := []string{"@@ " + itemPath(change) + " @@"}
	item := itemName(change)
	for _, side := range []struct {
		mark  string
		value any
	}{{"-", change.Before}, {"+", change.After}} {
		for _, line := range valueLines(side.mark, side.value, item) {
			if strings.Contains(line, security.Mask) {
				line += "  # " + hiddenText
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// textChangeLines shows a replaced multi-line string as its line diff.
func textChangeLines(change domain.Change, d textDiff) []string {
	lines := append([]string{"@@ " + itemPath(change) + ": " + d.summary("→") + " @@"}, d.lines...)
	if d.more > 0 {
		lines = append(lines, " … "+d.moreText()+" in JSON")
	}
	return lines
}

// listChangeLines shows a list without item keys as its removed and added
// items, up to prMaxListItems each.
func listChangeLines(change domain.Change, items listItems) []string {
	lines := []string{fmt.Sprintf("@@ %s: list changed (%d → %d items; %d added, %d removed%s) @@", itemPath(change), len(change.Before.([]any)), len(change.After.([]any)), len(items.added), len(items.removed), items.reordered)}
	for _, group := range items.groups() {
		for i, item := range group.items {
			if i == prMaxListItems {
				lines = append(lines, fmt.Sprintf(" … %d more %s item(s) in JSON", len(group.items)-i, group.name))
				break
			}
			lines = append(lines, valueLines(group.mark, []any{item}, "")...)
		}
	}
	return lines
}

// valueChangeLines shows an added, removed or replaced value. An added or
// removed object or list names its line count. Two one-line values start
// shortly before their first difference when either is long. Each side is
// printed once.
func valueChangeLines(change domain.Change) []string {
	header := "@@ " + itemPath(change) + " @@"
	item := itemName(change)
	var before, after string
	var bok, aok bool
	if change.Operation != domain.OperationAdd {
		before, bok = valueText(change.Before, item)
	}
	if change.Operation != domain.OperationRemove {
		after, aok = valueText(change.After, item)
	}
	if value, ok := addedOrRemoved(change); ok {
		text, textOK := after, aok
		if change.Operation == domain.OperationRemove {
			text, textOK = before, bok
		}
		switch value.(type) {
		case map[string]any, []any:
			if textOK {
				header = fmt.Sprintf("@@ %s: %s (%d lines) @@", itemPath(change), map[domain.Operation]string{domain.OperationAdd: "added", domain.OperationRemove: "removed"}[change.Operation], len(textLines(text)))
			}
		}
	}
	lines := []string{header}
	if change.Operation == domain.OperationReplace && bok && aok && oneLine(before) && oneLine(after) {
		before, after = strings.TrimSuffix(before, "\n"), strings.TrimSuffix(after, "\n")
		before, after = domain.PairFrom(before, after, domain.RunesAtLeast(before, after, prLineWidth), prLineWidth/4)
		return append(lines, "-"+before, "+"+after)
	}
	if change.Operation != domain.OperationAdd {
		lines = append(lines, markLines("-", change.Before, before, bok)...)
	}
	if change.Operation != domain.OperationRemove {
		lines = append(lines, markLines("+", change.After, after, aok)...)
	}
	return lines
}

func addedOrRemoved(change domain.Change) (any, bool) {
	switch change.Operation {
	case domain.OperationAdd:
		return change.After, true
	case domain.OperationRemove:
		return change.Before, true
	}
	return nil, false
}

func oneLine(text string) bool {
	return !strings.Contains(strings.TrimSuffix(text, "\n"), "\n")
}

// itemName returns the name of a whole list item compared by name, such as
// an added env var, whose YAML starts with the field that names it, or "".
func itemName(change domain.Change) string {
	if len(change.ItemNames) == 0 {
		return ""
	}
	last := change.Path[strings.LastIndex(change.Path, "/")+1:]
	if _, err := strconv.Atoi(last); err != nil {
		return ""
	}
	return change.ItemNames[len(change.ItemNames)-1]
}

// valueText prints a value; a whole list item named item starts with the
// field that names it in its change path.
func valueText(value any, item string) (string, bool) {
	if item != "" {
		return yamlview.ItemText(value, item)
	}
	return yamlview.Text(value)
}

// valueLines marks the YAML lines of a value, up to prMaxValueLines.
func valueLines(mark string, value any, item string) []string {
	text, ok := valueText(value, item)
	return markLines(mark, value, text, ok)
}

// markLines marks the lines of text, the printed value, up to
// prMaxValueLines; without text (ok false) the value's JSON.
func markLines(mark string, value any, text string, ok bool) []string {
	if !ok {
		text = string(displayJSON(value))
	}
	lines := []string{}
	shown, more := bounded(textLines(text), prMaxValueLines)
	for _, line := range shown {
		lines = append(lines, mark+line)
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf(" … %d more line(s) in JSON", more))
	}
	return lines
}

// pairStarts starts a long removed line and the added line after it shortly
// before their first difference, so a change past the line width stays
// visible. A diff block cuts a marked line at prLineWidth runes (writeFence),
// which shortens a pair of prLineWidth runes or more without marks
// (domain.RunesAtLeast); line hunks were bounded at their own width.
func pairStarts(lines []string) []string {
	return domain.StartLongPairs(lines, prLineWidth, domain.RunesAtLeast)
}

// writeManifest shows a whole added or removed manifest, collapsed when it
// is longer than prInlineManifestLines so the list of changes stays readable.
func writeManifest(b *strings.Builder, res domain.ResourceDiff) {
	change := res.Changes[0]
	var lines []string
	var total int
	if view := viewChange(change, changeOptions{}); view.kind == changeWhole {
		value := view.whole.value
		text, ok := yamlview.Text(value)
		lines = markLines(view.whole.mark, value, text, ok)
		total = len(lines)
		if ok {
			total = len(textLines(text))
		}
	} else {
		lines = fieldDiffLines(change)
		total = len(lines)
	}
	if total <= prInlineManifestLines {
		writeDiffBlock(b, lines)
		return
	}
	fmt.Fprintf(b, "<details>\n<summary>%s manifest (%d lines)</summary>\n\n", prText(string(res.State), 40), total)
	writeDiffBlock(b, lines)
	b.WriteString("</details>\n\n")
}

// writeDiffBlock prints lines in a fenced diff block (writeFence).
func writeDiffBlock(b *strings.Builder, lines []string) {
	if len(lines) == 0 {
		return
	}
	writeFence(b, "", lines, prLineWidth)
	b.WriteString("\n")
}
