package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

// Full Markdown sections in reading order: header, verdict, errors, review
// overview, repository discovery, report issues, policies, image changes,
// render backends, graph discovery, one section per Application and the
// comparison boundaries.

func encodeMarkdown(v *reportView) ([]byte, error) {
	r := v.report
	var b strings.Builder
	fmt.Fprintf(&b, "# Diffaro preview\n\nStatus: **%s** · Changes found: **%t**\n\n", esc(string(r.Status)), r.ChangesFound)
	fmt.Fprintf(&b, "- Repository: `%s`\n- Base: `%s`\n- Head: `%s`\n- Comparison: `%s`\n- Coverage: `%s`\n- Redaction: `%s`\n\n", esc(r.Revisions.Repository), esc(r.Revisions.Base), esc(r.Revisions.Head), esc(r.Revisions.Mode), esc(r.Coverage), esc(r.Redaction))
	writeVerdict(&b, v.verdict)
	writeErrors(&b, v, true)
	writeMarkdownNavigation(&b, v)
	writeSelections(&b, r, "## Repository discovery")
	for _, issue := range v.reportIssues {
		writeIssue(&b, issue, "")
	}
	writePolicy(&b, r, false)
	writeImageSummary(&b, v.images)
	writeMarkdownBackends(&b, r.RenderBackends)
	writeMarkdownGraph(&b, r.Graph)
	last, first := reviewDestination{}, true
	for _, app := range v.ordered {
		dest := app.destination
		if first || dest != last {
			fmt.Fprintf(&b, "## Argo CD `%s` → destination `%s`\n\n", esc(knownDestination(dest.instance)), esc(dest.label()))
			last, first = dest, false
		}
		writeMarkdownApplication(&b, app)
	}
	if len(r.Applications) == 0 {
		b.WriteString("No Applications were compared.")
		if len(r.Issues) > 0 {
			b.WriteString(" See failures above.")
		}
		b.WriteString("\n\n")
	}
	if len(r.Limitations) > 0 {
		b.WriteString("## Comparison boundaries\n\n")
		for _, item := range r.Limitations {
			fmt.Fprintf(&b, "- %s\n", esc(item))
		}
	}
	markdown := htmlCodeSpans(b.String())
	if len(markdown) > MaxReportBytes {
		return nil, domain.LimitError(domain.CodeReportLimitExceeded, "report", "encoded Markdown report", "final report byte budget", int64(len(markdown)), MaxReportBytes, "bytes", false, 0)
	}
	return []byte(markdown), nil
}

func writeMarkdownNavigation(b *strings.Builder, v *reportView) {
	c := v.counts
	if v.coverage.markdownIncomplete() {
		b.WriteString("**Analysis coverage is incomplete.** Missing targets are not no changes; independent results remain available.\n\n")
	}
	fmt.Fprintf(b, "Applications: %s · Resources: %s · Failed/denied: %d · Skipped: %d · Incomplete resources: %d\n\n", stateCounts(c.apps, countedStates, true, " / "), stateCounts(c.resources, countedStates, true, " / "), c.failed, c.skipped, c.incomplete)
	fmt.Fprintf(b, "## Review overview\n\n%d apps to review · %d instance/destination groups. Counts concern target changes; moved destinations show base → head.\n\n", len(v.review), len(v.reviewGroups))
	if len(v.reviewGroups) > 0 {
		b.WriteString("| Argo CD | Destination cluster | Apps to review | Resources + / ~ / − / ? | Failed / skipped / incomplete |\n|---|---|---:|---|---|\n")
	}
	for i, g := range v.reviewGroups {
		if i == maxReviewGroups {
			fmt.Fprintf(b, "\n%d more groups in JSON.\n", len(v.reviewGroups)-i)
			break
		}
		gc := g.counts
		fmt.Fprintf(b, "| `%s` | `%s` | %d | %s | %d / %d / %d |\n", prText(knownDestination(g.destination.instance), 80), prText(g.destination.label(), 180), gc.applications, stateCounts(gc.resources, countedStates, false, " / "), gc.failed, gc.skipped, gc.incomplete)
	}
	b.WriteString("\nReview index: instance → destination → Application; open the Application for target/source/path evidence.\n\n")
	for i, a := range v.review {
		if i == maxReviewApps {
			fmt.Fprintf(b, "- %d more apps to review; see details or JSON.\n", len(v.review)-i)
			break
		}
		origin := ""
		if label := originLabels[a.FailureOrigin]; label != "" {
			origin = " (" + prText(strings.SplitN(label, ":", 2)[0], 80) + ")"
		}
		check := ""
		if showsAction(a.action) {
			check = " Check: " + prText(a.action, 240)
		}
		fmt.Fprintf(b, "- [`%s`](#%s) · `%s` · namespace `%s` · **%s / %s**%s · %s.%s\n", prText(a.id, 160), appAnchor(a.ID), prText(a.destination.label(), 160), prText(a.namespace, 120), prText(string(a.State), 40), prText(string(a.Execution), 40), origin, prText(reviewResources(a.ApplicationResult), 200), check)
		// The Errors section states each failure; the index adds skip reasons.
		if boundary := reviewBoundary(a); boundary != "" && a.SkipReason != "" {
			fmt.Fprintf(b, "  - %s\n", prText(boundary, 240))
		}
	}
	if len(v.review) == 0 {
		b.WriteString("No app changes or app analysis failures in this index; check report and graph coverage.\n")
	}
	b.WriteString("\n")
}

// requestedRevision is the reference app resolved revision from at run
// time, or "".
func requestedRevision(app domain.ApplicationResult, revision string) string {
	for _, target := range []*domain.TargetSummary{app.BaseTarget, app.HeadTarget} {
		if target != nil && target.Revision == revision && target.RequestedRevision != "" {
			return target.RequestedRevision
		}
	}
	return ""
}

// writeIssue writes one issue; requested names the reference its revision
// was resolved from at run time, if any.
func writeIssue(b *strings.Builder, i domain.Issue, requested string) {
	if i.Side != "" {
		app := ""
		if i.Application != nil {
			app = i.Application.String()
		}
		fmt.Fprintf(b, "Side: `%s` · Application: `%s` · repository: `%s`\n\n", esc(i.Side), esc(app), esc(i.Repository))
	}
	resolved := resolvedSuffix(requested, func(s string) string { return "`" + esc(s) + "`" })
	fmt.Fprintf(b, "**%s** · stage `%s` · revision `%s`%s\n\n%s\n\nRequired action: %s\n\n", esc(issueLabel(i)), esc(i.Stage), esc(i.Revision), resolved, esc(i.Reason), esc(i.Action))
	if fields := defectFields(i); len(fields) > 0 {
		fmt.Fprintf(b, "Diagnostic: %s\n\n", strings.Join(mapStrings(fields, func(s string) string { return "`" + esc(s) + "`" }), " · "))
	}
	if access := i.RequiredAccess; access != nil {
		fmt.Fprintf(b, "Required access: %s\n\n", strings.Join(accessFields(access, func(s string, _ int) string { return esc(s) }), " · "))
		if access.Object != "" {
			fmt.Fprintf(b, "RBAC object: `%s`\n\n", esc(access.Object))
		}
		if access.Scope != "" {
			fmt.Fprintf(b, "Access scope: %s\n\n", esc(access.Scope))
		}
	}
}

func writeMarkdownBackends(b *strings.Builder, backends []domain.BackendInfo) {
	for _, backend := range backends {
		fmt.Fprintf(b, "Render backend: `%s` · instance `%s` · context `%s` · verification `%s`\n\n", esc(backend.Kind), esc(backend.Instance), esc(backend.ContextID), esc(backend.Verification))
		if backend.HelmVersion != "" {
			fmt.Fprintf(b, "Helm: `%s` · SHA-256: `%s`\n\n", esc(backend.HelmVersion), esc(backend.HelmSHA256))
		}
		if len(backend.HelmEnvironment) > 0 {
			fmt.Fprintf(b, "Helm environment (names only): `%s`\n\n", esc(strings.Join(backend.HelmEnvironment, ", ")))
		}
		b.WriteString(backendVersionLine(esc(backend.Version), esc(backend.Compatibility)))
	}
}

// backendVersionLine names the backend version and compatibility that are
// set, or returns "" when neither is.
func backendVersionLine(version, compatibility string) string {
	var parts []string
	if version != "" {
		parts = append(parts, "Version: `"+version+"`")
	}
	if compatibility != "" {
		parts = append(parts, "compatibility: `"+compatibility+"`")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ") + "\n\n"
}

func writeMarkdownGraph(b *strings.Builder, graph *domain.GraphSummary) {
	if graph == nil {
		return
	}
	fmt.Fprintf(b, "Graph discovery: base complete `%t` · head complete `%t`\n\n", graph.BaseComplete, graph.HeadComplete)
	// Edges that changed or are unknown are listed with their base and head
	// evidence; unchanged edges are counted, and JSON lists them.
	type link struct{ parent, child domain.ApplicationID }
	shown := map[link]bool{}
	unchanged := 0
	for _, change := range graph.Changes {
		if change.State == domain.StateUnchanged {
			unchanged++
			continue
		}
		if len(shown) == 200 {
			fmt.Fprint(b, "Additional graph changes omitted; use JSON.\n\n")
			break
		}
		shown[link{change.Parent, change.Child}] = true
		fmt.Fprintf(b, "- Edge **%s**: `%s` → `%s`\n", esc(string(change.State)), esc(change.Parent.String()), esc(change.Child.String()))
	}
	if unchanged > 0 {
		fmt.Fprintf(b, "- %d unchanged edge(s); JSON lists them in `graph.changes`, with their evidence in `graph.baseEdges` and `graph.headEdges`.\n", unchanged)
	}
	b.WriteString("\n")
	for _, side := range []struct {
		name  string
		edges []domain.GraphEdge
	}{{"base", graph.BaseEdges}, {"head", graph.HeadEdges}} {
		for _, edge := range side.edges {
			if shown[link{edge.Parent, edge.Child}] {
				fmt.Fprintf(b, "- %s: `%s` → `%s` · %s · route `%s` · `%s` @ `%s`\n", side.name, esc(edge.Parent.String()), esc(edge.Child.String()), esc(edge.Method), esc(edge.Routing), esc(edge.Repository), esc(edge.Revision))
			}
		}
	}
	b.WriteString("\n")
}

// writeMarkdownApplication writes one Application's evidence and changes.
func writeMarkdownApplication(b *strings.Builder, app *appView) {
	fmt.Fprintf(b, "<a id=\"%s\"></a>\n\n", appAnchor(app.ID))
	fmt.Fprintf(b, "## %s\n\nApplication: **%s** · Execution: **%s**\n\nProject: `%s` · Cluster: `%s` · Namespace: `%s`\n\n", esc(app.id), esc(string(app.State)), esc(string(app.Execution)), esc(app.Project), esc(app.Cluster), esc(app.Namespace))
	if showsAction(app.action) {
		fmt.Fprintf(b, "Check: %s\n\n", esc(app.action))
	}
	if app.PlaceholderInput != nil {
		fmt.Fprintf(b, "%s\n\n", placeholderText(app.PlaceholderInput, func(p string) string { return "`" + esc(p) + "`" }))
	}
	fmt.Fprintf(b, "Root: `%s`\n\nInputs: `%s` · origin `%s` · release `%s`\n\nValue files (in order): `%s`\n\nParameters (names only): `%s` · file parameters (names only): `%s`\n\nInline values: `%t` · valuesObject: `%t`\n\n", esc(app.Root.String()), esc(app.Inputs.Path), esc(app.Inputs.Origin), esc(app.Inputs.ReleaseName), esc(strings.Join(app.Inputs.ValueFiles, ", ")), esc(strings.Join(app.Inputs.Parameters, ", ")), esc(strings.Join(app.Inputs.FileParameters, ", ")), app.Inputs.InlineValues, app.Inputs.ValuesObject)
	writeMarkdownTargets(b, app.ApplicationResult)
	writeSourceContext(b, app.ApplicationResult, false)
	if len(app.SpecChanges) > 0 {
		b.WriteString("Desired Application spec changes:\n\n")
		for i, change := range app.SpecChanges {
			if i >= 50 {
				b.WriteString("Additional spec changes omitted; use JSON.\n\n")
				break
			}
			writeMarkdownChange(b, change, false)
		}
		b.WriteString("\n")
	}
	writeAppInputChanges(b, app.ApplicationResult)
	if app.Pruning != nil {
		writePruningCandidates(b, fmt.Sprintf("Pruning: **%s** · %d resource candidates. %s\n\n", esc(app.Pruning.Certainty), len(app.Pruning.Resources), esc(app.Pruning.Reason)), app.Pruning.Resources, 20, "Additional pruning candidates are in JSON.\n\n", func(id domain.ResourceID) string { return esc(resourceLabel(id)) })
	} else if app.State == domain.StateRemoved {
		b.WriteString(pruningUnknown + ".\n\n")
	}
	if app.SkipReason != "" {
		fmt.Fprintf(b, "Skipped: %s\n\n", esc(app.SkipReason))
	}
	for _, issue := range app.issues {
		writeIssue(b, issue, requestedRevision(app.ApplicationResult, issue.Revision))
	}
	// Changed, unknown and incompletely compared resources get a section;
	// unchanged ones are named in one line.
	var unchanged []string
	written := 0
	for i, res := range app.Resources {
		if res.State == domain.StateUnchanged && !resourceComparisonIncomplete(res) {
			unchanged = append(unchanged, "`"+esc(res.ID.Kind+"/"+res.ID.Namespace+"/"+res.ID.Name)+"`")
			continue
		}
		if written == 200 {
			fmt.Fprintf(b, "Remaining %d resources omitted from Markdown; use JSON for the full report.\n\n", len(app.Resources)-i)
			break
		}
		writeMarkdownResource(b, res)
		written++
	}
	if len(unchanged) > 0 {
		names, more := bounded(unchanged, 20)
		fmt.Fprintf(b, "Unchanged resources (%d): %s", len(unchanged), strings.Join(names, ", "))
		if more > 0 {
			fmt.Fprintf(b, ", %d more in JSON", more)
		}
		b.WriteString(".\n\n")
	}
}

func writeMarkdownTargets(b *strings.Builder, app domain.ApplicationResult) {
	for _, side := range []struct {
		name   string
		target *domain.TargetSummary
	}{{"base", app.BaseTarget}, {"head", app.HeadTarget}} {
		if side.target == nil {
			continue
		}
		t := side.target
		if t.SkipReason != "" {
			fmt.Fprintf(b, "%s target skipped: %s\n\n", side.name, esc(t.SkipReason))
		}
		fmt.Fprintf(b, "%s target: `%s` @ `%s`%s · rule `%s` · execution `%s`\n\nPath: `%s`\n\nDestination: `%s` / `%s` · project `%s` · input `%s` · release `%s` · origin `%s`\n\nValue files: `%s` · parameters: `%s` · file parameters: `%s` · inline values `%t` · valuesObject `%t`\n\n", side.name, esc(t.Repository), esc(t.Revision), resolvedSuffix(t.RequestedRevision, func(s string) string { return "`" + esc(s) + "`" }), esc(t.RevisionRule), esc(string(t.Execution)), esc(domain.JoinGraphPath(t.GraphPath, " → ")), esc(t.Cluster), esc(t.Namespace), esc(t.Project), esc(t.Inputs.Path), esc(t.Inputs.ReleaseName), esc(t.Inputs.Origin), esc(strings.Join(t.Inputs.ValueFiles, ", ")), esc(strings.Join(t.Inputs.Parameters, ", ")), esc(strings.Join(t.Inputs.FileParameters, ", ")), t.Inputs.InlineValues, t.Inputs.ValuesObject)
	}
}

func writeMarkdownResource(b *strings.Builder, res domain.ResourceDiff) {
	fmt.Fprintf(b, "### %s/%s/%s — %s\n\nComparison: `%s`\n\n", esc(res.ID.Kind), esc(res.ID.Namespace), esc(res.ID.Name), esc(string(res.State)), esc(string(res.Comparison)))
	if text := incompleteText(res, func(p string) string { return "`" + esc(p) + "`" }); text != "" {
		fmt.Fprintf(b, "- %s\n", text)
		if len(res.Changes) == 0 {
			b.WriteString("\n")
		}
	}
	for j, change := range res.Changes {
		if j >= 50 {
			fmt.Fprint(b, "Additional changes omitted from Markdown; use JSON.\n\n")
			break
		}
		writeMarkdownChange(b, change, true)
	}
	if len(res.Changes) > 0 {
		b.WriteString("\n")
	}
}

// listItemWidth is the length above which a list without item keys is shown
// as its added and removed items in full Markdown.
const listItemWidth = 400

// writeMarkdownChange writes one change as a list item. A resource change
// names its operation, may show a list as its items and shows a whole
// resource manifest by its familiar settings; a desired spec change does
// none of these.
func writeMarkdownChange(b *strings.Builder, change domain.Change, resource bool) {
	path := "`" + esc(itemPath(change)) + "`"
	operation, label := "", path
	options := changeOptions{objectLines: true}
	if resource {
		operation = " (" + esc(string(change.Operation)) + ")"
		label = path + operation
		options.lists, options.listWidth = true, listItemWidth
	}
	view := viewChange(change, options)
	switch {
	case view.kind == changeWhole && resource && view.whole.isManifest:
		writeWholeResourceHighlights(b, change, view.whole)
	case view.kind == changeHidden:
		fmt.Fprintf(b, "- %s%s: %s\n", path, operation, hiddenText)
	case view.kind == changeLines:
		writeTextDiff(b, "", label, view.lines, 400)
	case view.kind == changeList:
		writeListItems(b, "", label, change, view.items, 4000)
	default:
		before, after := shortPair(displayJSON(change.Before), displayJSON(change.After))
		before, after = markdownPair(change, before, after)
		fmt.Fprintf(b, "- %s%s: %s → %s\n", path, operation, before, after)
	}
}

// markdownPair shows the escaped before and after values of a change as code
// spans, and the side an added or removed value lacks as absentText.
func markdownPair(change domain.Change, before, after string) (string, string) {
	before, after = "`"+before+"`", "`"+after+"`"
	if change.Operation == domain.OperationAdd {
		before = esc(absentText)
	}
	if change.Operation == domain.OperationRemove {
		after = esc(absentText)
	}
	return before, after
}

// writeListItems prints a whole-list change as its removed and added items,
// each up to width characters.
func writeListItems(b *strings.Builder, indent, label string, change domain.Change, items listItems, width int) {
	fmt.Fprintf(b, "%s- %s: list changed (%d → %d items; %d added, %d removed%s)\n", indent, label, len(change.Before.([]any)), len(change.After.([]any)), len(items.added), len(items.removed), items.reordered)
	for _, group := range items.groups() {
		for i, item := range group.items {
			if i == 8 {
				fmt.Fprintf(b, "%s  - %d more %s item(s) in JSON\n", indent, len(group.items)-i, group.name)
				break
			}
			data := displayJSON(item)
			fmt.Fprintf(b, "%s  - %s: `%s`\n", indent, group.name, prText(string(data), width))
		}
	}
}

// writeTextDiff prints a text line diff as a fenced diff block under a list
// item.
func writeTextDiff(b *strings.Builder, indent, label string, d textDiff, width int) {
	fmt.Fprintf(b, "%s- %s: %s\n", indent, label, d.summary("→"))
	inner := indent + "  "
	writeFence(b, inner, d.lines, width)
	if d.more > 0 {
		fmt.Fprintf(b, "%s- %s in JSON\n", inner, d.moreText())
	}
}

// writeWholeResourceHighlights shows familiar target settings of a whole
// added or removed manifest, whose full manifest is in JSON.
func writeWholeResourceHighlights(b *strings.Builder, change domain.Change, w wholeView) {
	fmt.Fprintf(b, "- %s target manifest (%s; full manifest in JSON)\n", w.side, esc(string(change.Operation)))
	for _, field := range w.fields {
		if field.terminalOnly {
			continue
		}
		raw := displayJSON(field.value)
		fmt.Fprintf(b, "  - %s target `/%s`: `%s`\n", w.side, field.path, prText(string(raw), 240))
	}
	for _, containers := range w.containers {
		for i, c := range containers {
			if i == 6 {
				b.WriteString("  - More containers in JSON.\n")
				break
			}
			if container, ok := c.(map[string]any); ok {
				name := displayJSON(container["name"])
				image := displayJSON(container["image"])
				fmt.Fprintf(b, "  - %s container `%s`: image `%s`\n", w.side, prText(string(name), 80), prText(string(image), 200))
			}
		}
	}
}
