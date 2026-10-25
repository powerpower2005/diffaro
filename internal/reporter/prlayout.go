package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

// PR Markdown sections in reading order: the headline, Needs
// attention, coverage warnings (written in pr.go), changes by Application and
// the Applications without changes. Inputs and comparison details are collapsed (pr.go); diff
// block lines are built in prdiff.go.

// Bounds of the visible change list. Every count past them is named.
const (
	prChangeBudget    = 32 << 10
	prMaxResources    = 50
	prMaxAppSections  = 30
	prMaxFields       = 20
	prMaxValueLines   = 40
	prMaxListItems    = 8
	prLineWidth       = 240
	prMaxNamedPerLine = 10
	prMaxCauses       = 12
	prMaxCauseApps    = 5
	// A whole added or removed manifest longer than this is collapsed.
	prInlineManifestLines = 20
)

// writePRHeadline states the result in one heading and one status line.
func writePRHeadline(b *strings.Builder, v *reportView) {
	report, counts := v.report, v.counts
	resources := counts.resources[domain.StateAdded] + counts.resources[domain.StateChanged] + counts.resources[domain.StateRemoved]
	apps := counts.appsWithChanges
	switch {
	case report.Status == domain.StatusFailed || report.Status == domain.StatusDenied || report.Status == domain.StatusInvalidConfiguration || report.Status == domain.StatusCancelled:
		// No useful result: target changes are unknown, whatever was counted.
		fmt.Fprintf(b, "## Diffaro: %s, target changes are unknown\n\n", prText(statusLabels[report.Status], 60))
	case resources > 0:
		parts := []string{}
		for _, state := range changeStates {
			if n := counts.resources[state]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, state))
			}
		}
		fmt.Fprintf(b, "## Diffaro: %s in %s (%s)\n\n", plural(resources, "resource change", "resource changes"), plural(apps, "Application", "Applications"), strings.Join(parts, ", "))
	case apps > 0:
		fmt.Fprintf(b, "## Diffaro: %s\n\n", plural(apps, "Application change", "Application changes"))
	case counts.failed > 0 || counts.skipped > 0:
		b.WriteString("## Diffaro: no target changes in the compared Applications\n\n")
	default:
		b.WriteString("## Diffaro: no target changes\n\n")
	}
	label := v.verdict.result()
	fmt.Fprintf(b, "**%s** · base `%s` → head `%s`", prText(strings.ToUpper(label[:1])+label[1:], 120), prText(shortSHA(report.Revisions.Base), 40), prText(shortSHA(report.Revisions.Head), 40))
	fmt.Fprintf(b, " · %s compared", plural(counts.compared, "Application", "Applications"))
	unknown := []string{}
	if counts.failed > 0 {
		unknown = append(unknown, fmt.Sprintf("%d failed", counts.failed))
	}
	if counts.skipped > 0 {
		unknown = append(unknown, fmt.Sprintf("%d skipped", counts.skipped))
	}
	if counts.incomplete > 0 {
		unknown = append(unknown, plural(counts.incomplete, "resource", "resources")+" compared incompletely")
	}
	if len(unknown) > 0 {
		fmt.Fprintf(b, " · **%s: their changes are unknown, not unchanged**", strings.Join(unknown, ", "))
	}
	b.WriteString("\n\n")
	// The heading counts changes, Needs attention names counted failures and
	// the full verdict is in the details; skipped and excluded Applications
	// stay next to the status.
	shown := false
	for _, line := range v.verdict.lines {
		if line.kind == verdictChanges || line.kind == verdictRules || line.kind == verdictCounted {
			continue
		}
		fmt.Fprintf(b, "%s\n", prText(line.text, 1200))
		shown = true
	}
	if shown {
		b.WriteString("\n")
	}
}

func shortSHA(sha string) string {
	if len(sha) == 40 && strings.Trim(sha, "0123456789abcdef") == "" {
		return sha[:7]
	}
	return sha
}

// writePRChanges shows each Application's changes under its own heading,
// then names the Applications that were not compared or did not change.
func writePRChanges(b *strings.Builder, g prGroups) {
	var section strings.Builder
	shownResources, totalResources, omittedApps := 0, 0, 0
	for i, app := range g.sections {
		if i >= prMaxAppSections || section.Len() > prChangeBudget {
			omittedApps++
			continue
		}
		writePRAppHeading(&section, app)
		if len(app.SpecChanges) > 0 {
			section.WriteString("**Application spec**\n\n")
			lines := []string{}
			for j, change := range app.SpecChanges {
				if j == prMaxFields {
					lines = append(lines, fmt.Sprintf(" … %d more Application spec change(s) in JSON", len(app.SpecChanges)-j))
					break
				}
				lines = append(lines, fieldDiffLines(change)...)
			}
			writeDiffBlock(&section, lines)
		}
		for _, res := range app.Resources {
			if res.State == domain.StateUnchanged && !resourceNotFullyCompared(res) {
				continue
			}
			totalResources++
			if shownResources == prMaxResources || section.Len() > prChangeBudget {
				continue
			}
			shownResources++
			writePRResource(&section, res)
		}
		writePRPruning(&section, app.ApplicationResult)
	}
	b.WriteString(section.String())
	if omittedApps > 0 {
		fmt.Fprintf(b, "%s with changes or incomplete results not shown here; see JSON or the full Markdown report.\n\n", plural(omittedApps, "more Application", "more Applications"))
	}
	if totalResources > shownResources {
		fmt.Fprintf(b, "%s not shown here; see JSON or the full Markdown report.\n\n", plural(totalResources-shownResources, "more resource", "more resources"))
	}
	writeNamedApps(b, "Not compared, changes unknown", g.unknown, func(app *appView) string {
		if app.Execution == domain.ExecutionSkipped {
			return "skipped: " + prText(app.SkipReason, 240)
		}
		return prText(string(app.Execution), 40) + ", see Needs attention"
	})
	writeNamedApps(b, "PR inputs changed, rendered targets unchanged", g.inputsOnly, nil)
	writeNamedApps(b, "No target changes", g.unchanged, nil)
}

func writePRAppHeading(b *strings.Builder, app *appView) {
	if app.State == "" {
		fmt.Fprintf(b, "### `%s`\n\n", prText(app.ID.Name, 180))
	} else {
		fmt.Fprintf(b, "### `%s` · %s\n\n", prText(app.ID.Name, 180), prText(string(app.State), 40))
	}
	fmt.Fprintf(b, "`%s` · cluster `%s` · namespace `%s` · [render context](#%s)\n\n", prText(app.id, 180), prText(app.destination.label(), 160), prText(app.namespace, 120), appAnchor(app.ID))
	if app.PlaceholderInput != nil {
		fmt.Fprintf(b, "%s\n\n", placeholderText(app.PlaceholderInput, func(p string) string { return "`" + prText(p, 240) + "`" }))
	}
	switch {
	case app.Execution == domain.ExecutionSkipped:
		fmt.Fprintf(b, "**Skipped:** %s. Changes not shown are unknown.\n\n", prText(app.SkipReason, 240))
	case notCompared(app.ApplicationResult):
		fmt.Fprintf(b, "**Not compared (%s)**, see Needs attention. Changes not shown are unknown.\n\n", prText(string(app.Execution), 40))
	}
	if showsAction(app.action) && !notCompared(app.ApplicationResult) {
		fmt.Fprintf(b, "Check: %s\n\n", prText(app.action, 240))
	}
}

func writePRResource(b *strings.Builder, res domain.ResourceDiff) {
	fmt.Fprintf(b, "**%s** %s `%s`", prText(string(res.State), 40), prText(res.ID.Kind, 90), prText(resourceName(res.ID), 240))
	if resourceComparisonIncomplete(res) {
		b.WriteString(" (comparison incomplete)")
	}
	b.WriteString("\n\n")
	if text := incompleteText(res, func(p string) string { return "`" + prText(p, 240) + "`" }); text != "" {
		fmt.Fprintf(b, "%s\n\n", text)
	}
	if len(res.Changes) == 1 && res.Changes[0].Path == "/" && (res.State == domain.StateAdded || res.State == domain.StateRemoved) {
		writeManifest(b, res)
		return
	}
	writeDiffBlock(b, resourceLines(res))
}

func writePRPruning(b *strings.Builder, app domain.ApplicationResult) {
	if app.Pruning == nil {
		if app.State == domain.StateRemoved {
			b.WriteString(pruningUnknown + ".\n\n")
		}
		return
	}
	writePruningCandidates(b, fmt.Sprintf("Pruning: %d candidate resource(s), **%s**. %s\n\n", len(app.Pruning.Resources), prText(app.Pruning.Certainty, 40), prText(app.Pruning.Reason, 240)), app.Pruning.Resources, 8, "- More candidates in JSON.\n", func(id domain.ResourceID) string { return prText(resourceLabel(id), 180) })
}

// writeNamedApps lists Applications on one line under a bold label.
func writeNamedApps(b *strings.Builder, label string, apps []*appView, detail func(*appView) string) {
	if len(apps) == 0 {
		return
	}
	names := []string{}
	shown, more := bounded(apps, prMaxNamedPerLine)
	for _, app := range shown {
		name := "`" + prText(app.id, 160) + "`"
		if detail != nil {
			name += " (" + detail(app) + ")"
		}
		names = append(names, name)
	}
	if more > 0 {
		names = append(names, fmt.Sprintf("%d more in JSON", more))
	}
	fmt.Fprintf(b, "**%s (%d):** %s\n\n", label, len(apps), strings.Join(names, ", "))
}

// writePRErrors names each failure cause on one line: what failed, whether
// this PR introduced it, and where. Where it happened, why, what to do and
// the required access are in the collapsed details, as in full Markdown.
func writePRErrors(b *strings.Builder, v *reportView) {
	groups, apps := v.errors, v.failedApps
	if len(groups) == 0 {
		return
	}
	if len(apps) > 0 {
		fmt.Fprintf(b, "## Needs attention: %s not compared\n\n", plural(len(apps), "Application", "Applications"))
	} else {
		b.WriteString("## Needs attention: the run did not complete\n\n")
	}
	for i, group := range groups {
		if i == prMaxCauses {
			fmt.Fprintf(b, "- %d more cause(s) in the error details.\n", len(groups)-i)
			break
		}
		fmt.Fprintf(b, "- **%s**", prText(causeTitle(group.issue), 80))
		if label := originLabels[group.origin]; label != "" {
			fmt.Fprintf(b, " (%s)", prText(strings.SplitN(label, ":", 2)[0], 80))
		}
		// Only Application members are named and counted; a run-level
		// member of the same cause is neither.
		names, more := []string{}, 0
		for _, member := range group.members {
			switch {
			case !member.appScoped:
			case len(names) == prMaxCauseApps:
				more++
			default:
				names = append(names, "`"+prText(member.app, 160)+"`")
			}
		}
		if more > 0 {
			names = append(names, fmt.Sprintf("%d more", more))
		}
		if len(names) > 0 {
			fmt.Fprintf(b, ": %s", strings.Join(names, ", "))
		} else {
			// A run-level failure has no Application; its reason says why
			// nothing was compared.
			fmt.Fprintf(b, ": %s", prText(group.members[0].reason, 240))
		}
		b.WriteString("\n")
	}
	if len(apps) > 0 {
		b.WriteString("\nTheir resource changes are unknown, not unchanged. Results of other Applications remain valid.\n")
	}
	b.WriteString("\n<details>\n<summary>Error details: where, why and what to do</summary>\n\n")
	writeErrors(b, v, false)
	b.WriteString("</details>\n\n")
}
