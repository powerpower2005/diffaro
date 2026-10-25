package reporter

import (
	"fmt"
	"sort"
	"strings"

	"diffaro/internal/domain"
)

var inputKinds = map[string]string{"valueFile": "value file", "chartFile": "chart file", "inlineValues": "inline values"}

func inputLabel(change domain.InputChange) string {
	label := inputKinds[change.Kind]
	if label == "" {
		label = change.Kind
	}
	label += ", " + string(change.State)
	if change.Comparison != domain.ComparisonPathOnly {
		label += fmt.Sprintf(", %d key change(s)", len(change.Changes))
	}
	if change.Comparison == domain.ComparisonIncomplete {
		label += ", comparison incomplete"
	}
	return label
}

type inputGroup struct {
	change domain.InputChange
	apps   []domain.ApplicationID
}

// inputGroups merges repository file changes read by several Applications;
// their content is the same for every reader. Inline values stay per
// Application. It also counts the Applications that read a changed input.
func inputGroups(ordered []*appView) ([]inputGroup, int) {
	index := map[string]int{}
	var groups []inputGroup
	for _, app := range ordered {
		for _, change := range app.InputChanges {
			key := change.Kind + "\x00" + change.Repository + "\x00" + change.Path
			if change.Kind == "inlineValues" {
				key += "\x00" + app.id
			}
			if i, ok := index[key]; ok {
				groups[i].apps = append(groups[i].apps, app.ID)
				continue
			}
			index[key] = len(groups)
			groups = append(groups, inputGroup{change: change, apps: []domain.ApplicationID{app.ID}})
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].change.Path != groups[j].change.Path {
			return groups[i].change.Path < groups[j].change.Path
		}
		return groups[i].apps[0].String() < groups[j].apps[0].String()
	})
	readers := map[domain.ApplicationID]bool{}
	for _, g := range groups {
		for _, id := range g.apps {
			readers[id] = true
		}
	}
	return groups, len(readers)
}

// writeInputSummary lists changed render inputs and the Applications that
// read them, so a reviewer sees which services one file change reaches.
func writeInputSummary(b *strings.Builder, v *reportView) {
	groups := v.inputs
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(b, "<details>\n<summary>PR inputs these Applications read: %d changed input(s), read by %d Application(s)</summary>\n\n", len(groups), v.inputReaders)
	for i, g := range groups {
		if i == 10 {
			fmt.Fprintf(b, "- %d more inputs are in the Application evidence and JSON.\n", len(groups)-i)
			break
		}
		names := []string{}
		shown, more := bounded(g.apps, 3)
		for _, id := range shown {
			names = append(names, "`"+prText(id.String(), 160)+"`")
		}
		if more > 0 {
			names = append(names, fmt.Sprintf("+%d more", more))
		}
		fmt.Fprintf(b, "- `%s` (%s) → %s\n", prText(g.change.Path, 200), prText(inputLabel(g.change), 120), strings.Join(names, ", "))
		writeInputKeys(b, "  ", g.change, 3, 120)
	}
	b.WriteString("\nInputs are shown next to target changes; they do not establish which target field a key produced.\n\n</details>\n\n")
}

func writeInputKeys(b *strings.Builder, indent string, change domain.InputChange, limit, width int) {
	if change.Reason != "" {
		fmt.Fprintf(b, "%s- Comparison: %s\n", indent, prText(change.Reason, 240))
	}
	for i, key := range change.Changes {
		if i == limit {
			fmt.Fprintf(b, "%s- %d more key change(s) in JSON.\n", indent, len(change.Changes)-i)
			break
		}
		label := "`" + prText(itemPath(key), 180) + "`"
		view := viewChange(key, changeOptions{objectLines: true, lists: true, listWidth: width})
		switch view.kind {
		case changeHidden:
			fmt.Fprintf(b, "%s- %s: %s\n", indent, label, hiddenText)
		case changeLines:
			writeTextDiff(b, indent, label, view.lines, min(width, 240))
		case changeList:
			writeListItems(b, indent, label, key, view.items, width)
		default:
			before, after := prPair(string(displayJSON(key.Before)), string(displayJSON(key.After)), width)
			before, after = markdownPair(key, before, after)
			fmt.Fprintf(b, "%s- %s: %s → %s\n", indent, label, before, after)
		}
	}
}

// writeAppInputChanges lists one Application's changed inputs in evidence.
func writeAppInputChanges(b *strings.Builder, app domain.ApplicationResult) {
	if len(app.InputChanges) == 0 {
		return
	}
	files, keys, width := 32, 50, 400
	b.WriteString("Changed render inputs:\n\n")
	for i, change := range app.InputChanges {
		if i == files {
			fmt.Fprintf(b, "- %d more inputs in JSON.\n", len(app.InputChanges)-i)
			break
		}
		fmt.Fprintf(b, "- `%s` (%s)\n", prText(change.Path, 200), prText(inputLabel(change), 120))
		writeInputKeys(b, "  ", change, keys, width)
	}
	b.WriteString("\n")
}
