package reporter

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"diffaro/internal/domain"
)

// Terminal is a bounded view of the same redacted report, not a machine format.
const MaxTerminalBytes = 64 << 10

const terminalTruncated = "\n[!] Output shortened to 64 KiB. Use --json-output FILE for the full report.\n"

type terminalWriter struct {
	b    strings.Builder
	full bool
}

func (w *terminalWriter) line(format string, args ...any) {
	if w.full {
		return
	}
	s := fmt.Sprintf(format, args...) + "\n"
	if w.b.Len()+len(s) > MaxTerminalBytes-len(terminalTruncated) {
		w.full = true
		return
	}
	w.b.WriteString(s)
}

// Escape every control/format character from untrusted content, including ANSI,
// carriage returns and bidi controls. Bound display text without splitting UTF-8.
func terminalText(s string) string {
	var b strings.Builder
	i := 0
	for _, r := range s {
		if i == 320 {
			b.WriteString("... [shortened]")
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
		i++
	}
	return b.String()
}

func terminalValue(value any) string {
	if s, ok := value.(string); ok {
		// Quote strings so empty strings, absent values and numbers remain distinct.
		return fmt.Sprintf("%q", terminalText(s))
	}
	b := displayJSON(value)
	return terminalText(string(b))
}

// terminalPair formats a before/after pair like terminalValue. When either
// side would be shortened, both start near their first difference, so values
// that differ only past the bound do not look identical.
func terminalPair(before, after any) (string, string) {
	raw := func(value any) string {
		if s, ok := value.(string); ok {
			return s
		}
		b := displayJSON(value)
		return string(b)
	}
	b, a := raw(before), raw(after)
	if !domain.RunesOver(b, a, 320) {
		return terminalValue(before), terminalValue(after)
	}
	start := domain.PairStart(b, a, true, 80)
	format := func(value any, s string) string {
		if start > 0 {
			s = "..." + string([]rune(s)[start:])
		}
		if _, ok := value.(string); ok {
			return fmt.Sprintf("%q", terminalText(s))
		}
		return terminalText(s)
	}
	return format(before, b), format(after, a)
}

func terminalMark(state domain.State) string { return "[" + stateSymbol(state) + "]" }

func terminalResource(id domain.ResourceID) string {
	group := ""
	if id.Group != "" {
		group = " (" + terminalText(id.Group) + ")"
	}
	return terminalText(id.Kind) + "/" + terminalText(id.Name) + group + " [cluster=" + terminalText(id.Cluster) + ", namespace=" + terminalText(id.Namespace) + "]"
}

// Terminal sections in order: summary and review overview, issues and
// policies, render context, Application details and coverage.
func encodeTerminal(v *reportView) []byte {
	w := &terminalWriter{}
	writeTerminalSummary(w, v)
	writeTerminalIssues(w, v)
	writeTerminalContext(w, v.report)
	writeTerminalApplications(w, v)
	writeTerminalCoverage(w, v.report)
	if w.full {
		w.b.WriteString(terminalTruncated)
	}
	return []byte(w.b.String())
}

// writeTerminalSummary states the result, the revisions, the counts and the
// review overview.
func writeTerminalSummary(w *terminalWriter, v *reportView) {
	r, c := v.report, v.counts
	w.line("Diffaro target preview")
	w.line("Status: %s | Changes found: %t | Exit code: %d", terminalText(string(r.Status)), r.ChangesFound, r.ExitCode())
	for _, line := range v.verdict.text() {
		w.line("%s", terminalText(line))
	}
	w.line("Repository: %s", terminalText(r.Revisions.Repository))
	w.line("Base: %s", terminalText(r.Revisions.Base))
	w.line("Head: %s", terminalText(r.Revisions.Head))
	w.line("Comparison: %s | Coverage: %s | Redaction: %s", terminalText(r.Revisions.Mode), terminalText(r.Coverage), terminalText(r.Redaction))
	w.line("Starting roots/apps (%d): %s", len(v.roots), terminalList(v.roots))
	w.line("")
	for _, row := range []struct {
		name   string
		counts map[domain.State]int
	}{{"Applications", c.apps}, {"Resources", c.resources}} {
		parts := []string{}
		for _, state := range changeStates {
			parts = append(parts, fmt.Sprintf("%s%d %s", stateSymbol(state), row.counts[state], state))
		}
		w.line("%s: %s, %d unchanged, %d unknown", row.name, strings.Join(parts, ", "), row.counts[domain.StateUnchanged], row.counts[domain.StateUnknown])
	}
	w.line("Analysis: %d failed/denied apps, %d skipped apps, %d incomplete resource comparisons", c.failed, c.skipped, c.incomplete)
	if v.coverage.terminalIncomplete() {
		w.line("[!] Analysis is incomplete. Missing targets do not mean no changes; independent results are preserved.")
	}
	if r.Graph != nil {
		w.line("Graph discovery: base complete=%t, head complete=%t", r.Graph.BaseComplete, r.Graph.HeadComplete)
		if v.coverage.graphIncomplete {
			w.line("[!] Graph discovery is incomplete. Missing branches are not evidence of removal.")
		}
	}
	w.line("Policy violations: %d | Issues: %d", len(r.PolicyViolations), len(v.issues))
	writeTerminalNavigation(w, v)
	w.line("Legend: [+] added  [~] changed  [-] removed  [!] attention  [?] unknown")
	w.line("Display text may be shortened. Full result: --json-output FILE (diff) or --format json.")
}

func writeTerminalNavigation(w *terminalWriter, v *reportView) {
	w.line("\nReview overview: %d apps to review | %d instance/destination groups", len(v.review), len(v.reviewGroups))
	for i, g := range v.reviewGroups {
		if i == maxReviewGroups {
			w.line("  +%d more groups; use JSON.", len(v.reviewGroups)-i)
			break
		}
		gc := g.counts
		w.line("  %s -> %s | apps=%d | resources %s | failed=%d skipped=%d incomplete=%d", terminalText(knownDestination(g.destination.instance)), terminalText(g.destination.label()), gc.applications, stateCounts(gc.resources, countedStates, true, " "), gc.failed, gc.skipped, gc.incomplete)
	}
	if len(v.review) == 0 {
		w.line("No app changes or app analysis failures in this index; check report and graph coverage above.")
		return
	}
	w.line("Review index (instance -> destination -> Application)")
	for i, a := range v.review {
		if i == maxReviewApps {
			w.line("  +%d more apps to review; details below or JSON.", len(v.review)-i)
			break
		}
		w.line("  %s %s -> %s -> %s | %s/%s | namespace=%s | %s", terminalMark(a.State), terminalText(a.ID.Instance), terminalText(a.destination.label()), terminalText(a.ID.Namespace+"/"+a.ID.Name), terminalText(string(a.State)), terminalText(string(a.Execution)), terminalText(a.namespace), terminalText(reviewResources(a.ApplicationResult)))
		if showsAction(a.action) {
			w.line("    Check: %s", terminalText(a.action))
		}
		if boundary := reviewBoundary(a); boundary != "" {
			w.line("    %s", terminalText(boundary))
		}
	}
}

// writeTerminalIssues lists issues with their required actions, then policy
// violations.
func writeTerminalIssues(w *terminalWriter, v *reportView) {
	r := v.report
	if len(v.issues) > 0 {
		w.line("\nIssues and required actions")
		requested := requestedRevisions(r)
		for _, issue := range v.issues {
			ref := ""
			if issue.Application != nil {
				ref = requested[issue.Application.String()+"\x00"+issue.Revision]
			}
			terminalIssue(w, issue, ref)
			if w.full {
				break
			}
		}
	}
	for _, violation := range r.PolicyViolations {
		w.line("[!] Policy: %s", terminalText(violation))
	}
	if r.Policy != nil {
		w.line("Policy evaluation: %s | Scope: %s", terminalText(r.Policy.Evaluation), terminalText(r.Policy.Scope))
		for _, violation := range r.Policy.Violations {
			w.line("[!] %s / %s: %s", terminalText(violation.PolicyID), terminalText(violation.RuleID), terminalResource(violation.Resource))
			w.line("    App: %s | Reason: %s", terminalText(violation.Application.String()), terminalText(violation.Reason))
			w.line("    Action: %s", terminalText(violation.Action))
		}
	}
}

// writeTerminalContext names the render backends, selections and discovery
// scopes.
func writeTerminalContext(w *terminalWriter, r domain.Report) {
	for _, b := range r.RenderBackends {
		w.line("\nBackend: %s | Instance: %s | Verification: %s", terminalText(b.Kind), terminalText(b.Instance), terminalText(b.Verification))
		w.line("  Context: %s", terminalText(b.ContextID))
		if b.HelmVersion != "" {
			w.line("  Helm: %s | SHA-256: %s", terminalText(b.HelmVersion), terminalText(b.HelmSHA256))
		}
		if len(b.HelmEnvironment) > 0 {
			w.line("  Helm environment (names only): %s", terminalText(strings.Join(b.HelmEnvironment, ", ")))
		}
		if b.Version != "" {
			w.line("  Version: %s", terminalText(b.Version))
		}
		if b.Compatibility != "" {
			w.line("  Compatibility: %s", terminalText(b.Compatibility))
		}
	}
	for _, selection := range r.Selections {
		w.line("Selection: %s | Mode: %s | Boundary: %s", terminalText(selection.Source.String()), terminalText(selection.Mode), terminalText(selection.Boundary))
	}
	for _, discovery := range r.Discovery {
		if discovery.Unaffected > 0 || discovery.UnaffectedDescendants > 0 {
			w.line("Discovery: %s | Status: %s | Unaffected, not rendered: %d candidates, %d descendants (changed-path selection)", terminalText(discovery.Instance), terminalText(string(discovery.Status)), discovery.Unaffected, discovery.UnaffectedDescendants)
		} else {
			w.line("Discovery: %s | Status: %s", terminalText(discovery.Instance), terminalText(string(discovery.Status)))
		}
	}
}

// writeTerminalApplications details the Applications that need review,
// grouped by instance and destination; the others are counted.
func writeTerminalApplications(w *terminalWriter, v *reportView) {
	w.line("\nApplication and resource changes")
	if hidden := len(v.ordered) - len(v.review); hidden > 0 {
		w.line("%d unchanged apps omitted from details; unchanged resources are counted above.", hidden)
	}
	last, first := reviewDestination{}, true
	for _, a := range v.review {
		if w.full {
			break
		}
		dest := a.destination
		if first || dest != last {
			w.line("\nInstance: %s | Destination cluster: %s", terminalText(knownDestination(dest.instance)), terminalText(dest.label()))
			last, first = dest, false
		}
		terminalApplication(w, a)
	}
	if len(v.review) == 0 {
		w.line("No app or resource change details in the analyzed scope. Check analysis coverage above.")
	}
}

func terminalApplication(w *terminalWriter, a *appView) {
	w.line("\n%s App %s (%s, execution=%s)", terminalMark(a.State), terminalText(a.id), terminalText(string(a.State)), terminalText(string(a.Execution)))
	if label := originLabels[a.FailureOrigin]; label != "" {
		w.line("  Failure origin: %s", terminalText(label))
	}
	w.line("  Destination: cluster=%s, namespace=%s | Project: %s", terminalText(a.Cluster), terminalText(a.Namespace), terminalText(a.Project))
	if showsAction(a.action) {
		w.line("  Check: %s", terminalText(a.action))
	}
	if a.PlaceholderInput != nil {
		w.line("  [!] %s", placeholderText(a.PlaceholderInput, terminalText))
	}
	if a.SkipReason != "" {
		w.line("  [!] Skipped: %s", terminalText(a.SkipReason))
	}
	terminalRepeated(w, a.ApplicationResult)
	for _, change := range a.SpecChanges {
		terminalChange(w, "  App spec", change)
	}
	terminalInputChanges(w, a.ApplicationResult)
	terminalResources(w, a.Resources)
	if a.Pruning != nil {
		w.line("  Pruning: %s | %s", terminalText(a.Pruning.Certainty), terminalText(a.Pruning.Reason))
		for _, id := range a.Pruning.Resources {
			w.line("    Target: %s", terminalResource(id))
		}
		w.line("  %s", pruningLiveUnknown)
	} else if a.State == domain.StateRemoved {
		w.line("  %s", pruningUnknown)
	}
	w.line("  Root: %s", terminalText(a.Root.String()))
	if len(a.GraphPath) > 0 {
		w.line("  Graph path: %s", terminalText(domain.JoinGraphPath(a.GraphPath, " -> ")))
	}
	if a.State == domain.StateAdded && a.BaseTarget == nil && len(a.BaseSources) == 0 {
		w.line("  Base target: absent (new Application)")
	}
	terminalTarget(w, "Base", a.BaseTarget, a.BaseSources)
	terminalTarget(w, "Head", a.HeadTarget, a.HeadSources)
	if a.State == domain.StateRemoved && a.HeadTarget == nil && len(a.HeadSources) == 0 {
		w.line("  Head target: absent (removed Application)")
	}
	if a.BaseTarget == nil && a.HeadTarget == nil && len(a.BaseSources) == 0 && len(a.HeadSources) == 0 {
		terminalInputs(w, a.Inputs)
	}
}

// terminalResources shows changed and incompletely compared resources in
// identity order.
func terminalResources(w *terminalWriter, resources []domain.ResourceDiff) {
	// Each identity key is computed once; the stable sort of indexes gives
	// the order of a stable sort of the resources.
	keys := make([]string, len(resources))
	order := make([]int, len(resources))
	for i, resource := range resources {
		keys[i], order[i] = resource.ID.String(), i
	}
	sort.SliceStable(order, func(i, j int) bool { return keys[order[i]] < keys[order[j]] })
	for _, i := range order {
		resource := resources[i]
		if w.full {
			break
		}
		if resource.State == domain.StateUnchanged && !resourceComparisonIncomplete(resource) {
			continue
		}
		w.line("  %s %s (%s, comparison=%s)", terminalMark(resource.State), terminalResource(resource.ID), terminalText(string(resource.State)), terminalText(string(resource.Comparison)))
		if text := incompleteText(resource, terminalText); text != "" {
			w.line("    %s", text)
		}
		for _, change := range resource.Changes {
			terminalResourceChange(w, change)
			if w.full {
				break
			}
		}
	}
}

// writeTerminalCoverage lists graph edge changes and what the comparison
// does not cover.
func writeTerminalCoverage(w *terminalWriter, r domain.Report) {
	if r.Graph != nil && len(r.Graph.Changes) > 0 {
		w.line("\nGraph edge changes")
		for _, edge := range r.Graph.Changes {
			if edge.State != domain.StateUnchanged {
				w.line("%s %s -> %s (%s)", terminalMark(edge.State), terminalText(edge.Parent.String()), terminalText(edge.Child.String()), terminalText(string(edge.State)))
			}
		}
	}
	if r.Policy != nil {
		for _, limitation := range r.Policy.Limitations {
			w.line("[?] Policy coverage: %s", terminalText(limitation))
		}
	}
	// Standing boundaries repeat in every report of their kind; count them
	// so this run's limitations stand out.
	standing := 0
	for _, limitation := range r.Limitations {
		if domain.StandingLimitation(limitation) {
			standing++
			continue
		}
		w.line("[?] Coverage: %s", terminalText(limitation))
	}
	if standing > 0 {
		w.line("[?] Coverage: %d standard boundaries of this comparison kind apply; --format markdown and the JSON limitations list them.", standing)
	}
}

// terminalResourceChange shows one resource change. Whole-resource changes
// retain their full manifests in JSON; show familiar target settings rather
// than an unreadable JSON blob in the terminal.
func terminalResourceChange(w *terminalWriter, change domain.Change) {
	view := viewChange(change, terminalChangeOptions)
	if view.kind != changeWhole || !view.whole.isManifest {
		terminalView(w, "    ", change, view)
		return
	}
	whole := view.whole
	w.line("    %s target manifest (%s; full manifest in JSON)", whole.side, terminalText(string(change.Operation)))
	for _, field := range whole.fields {
		w.line("    /%s: %s", field.path, terminalValue(field.value))
	}
	for _, containers := range whole.containers {
		for _, c := range containers {
			if container, ok := c.(map[string]any); ok {
				w.line("    Container %s: image=%s", terminalValue(container["name"]), terminalValue(container["image"]))
			}
		}
	}
}

// terminalIssue writes one issue; requested names the reference its
// revision was resolved from at run time, if any.
func terminalIssue(w *terminalWriter, issue domain.Issue, requested string) {
	w.line("[!] %s%s", terminalText(issue.Code), terminalText(labeled(" | ", "Stage", issue.Stage, "Side", issue.Side, "Cause", issue.ReasonCode)))
	if issue.Application != nil {
		w.line("    App: %s", terminalText(issue.Application.String()))
	}
	if issue.Repository != "" || issue.Revision != "" {
		w.line("    Source: %s @ %s%s", terminalText(issue.Repository), terminalText(issue.Revision), resolvedSuffix(requested, terminalText))
	}
	w.line("    Reason: %s", terminalText(issue.Reason))
	w.line("    Action: %s", terminalText(issue.Action))
	if fields := defectFields(issue); len(fields) > 0 {
		w.line("    Diagnostic: %s", terminalText(strings.Join(fields, " | ")))
	}
	if access := issue.RequiredAccess; access != nil {
		w.line("    Required access: %s | Instance: %s | Credential env: %s", terminalText(access.System), terminalText(access.Instance), terminalText(access.CredentialEnv))
		w.line("    Resource: %s | Action: %s | Target: %s | Object: %s | Scope: %s", terminalText(access.Resource), terminalText(access.Action), terminalText(access.Target), terminalText(access.Object), terminalText(access.Scope))
	}
}

var terminalChangeOptions = changeOptions{objectLines: true, lists: true, listWidth: 320}

// terminalChange shows one change after prefix as viewChange decides.
func terminalChange(w *terminalWriter, prefix string, change domain.Change) {
	terminalView(w, prefix, change, viewChange(change, terminalChangeOptions))
}

// terminalView shows one change after prefix as view; a whole resource or
// Application spec reads as a before/after pair.
func terminalView(w *terminalWriter, prefix string, change domain.Change, view changeView) {
	path := terminalText(itemPath(change))
	switch view.kind {
	case changeHidden:
		w.line("%s %s: %s", prefix, path, hiddenText)
	case changeLines:
		text := view.lines
		w.line("%s %s: %s", prefix, path, text.summary("->"))
		for _, line := range text.lines {
			w.line("%s   %s", prefix, terminalText(line))
		}
		if text.more > 0 {
			w.line("%s   ... %s; see JSON", prefix, text.moreText())
		}
	case changeList:
		items := view.items
		w.line("%s %s: list changed (%d -> %d items; %d added, %d removed%s)", prefix, path, len(change.Before.([]any)), len(change.After.([]any)), len(items.added), len(items.removed), items.reordered)
		for _, group := range items.groups() {
			for i, item := range group.items {
				if i >= 8 {
					w.line("%s   %s ... %d more; see JSON", prefix, group.mark, len(group.items)-i)
					break
				}
				w.line("%s   %s %s", prefix, group.mark, terminalValue(item))
			}
		}
	default:
		before, after := terminalPair(change.Before, change.After)
		if change.Operation == domain.OperationAdd {
			before = absentText
		}
		if change.Operation == domain.OperationRemove {
			after = absentText
		}
		redacted := ""
		if change.Redacted {
			redacted = " [redacted]"
		}
		w.line("%s %s: %s -> %s%s", prefix, path, before, after, redacted)
	}
}

func terminalTarget(w *terminalWriter, side string, target *domain.TargetSummary, sources []domain.SourceSummary) {
	if target != nil {
		w.line("  %s target: execution=%s | %s @ %s%s", side, terminalText(string(target.Execution)), terminalText(target.Repository), terminalText(target.Revision), resolvedSuffix(target.RequestedRevision, terminalText))
		w.line("    Destination: cluster=%s, namespace=%s | Project: %s", terminalText(target.Cluster), terminalText(target.Namespace), terminalText(target.Project))
		if target.SkipReason != "" {
			w.line("    [!] %s", terminalText(target.SkipReason))
		}
		if len(sources) == 0 {
			sources = target.Sources
		}
		if len(sources) == 0 {
			terminalInputs(w, target.Inputs)
		}
	}
	for _, source := range sources {
		w.line("  %s source %d: %s @ %s | Rule: %s | Ref: %s", side, source.Position, terminalText(source.Repository), terminalText(source.Revision), terminalText(source.Rule), terminalText(source.Ref))
		terminalInputs(w, source.Inputs)
		if w.full {
			break
		}
	}
}

func terminalInputs(w *terminalWriter, input domain.InputSummary) {
	if input.Path == "" && input.ReleaseName == "" && len(input.ValueFiles) == 0 && len(input.Parameters) == 0 && len(input.FileParameters) == 0 && !input.InlineValues && !input.ValuesObject && input.Origin == "" {
		return
	}
	w.line("    Inputs: path=%s, release=%s, origin=%s", terminalText(input.Path), terminalText(input.ReleaseName), terminalText(input.Origin))
	w.line("    Values: files=%s, inline=%t, valuesObject=%t", terminalList(input.ValueFiles), input.InlineValues, input.ValuesObject)
	if len(input.Parameters) > 0 || len(input.FileParameters) > 0 {
		w.line("    Parameters: %s | File parameters: %s", terminalList(input.Parameters), terminalList(input.FileParameters))
	}
}

func terminalInputChanges(w *terminalWriter, app domain.ApplicationResult) {
	for _, change := range app.InputChanges {
		w.line("  Input %s (%s)", terminalText(change.Path), terminalText(inputLabel(change)))
		if change.Reason != "" {
			w.line("    Comparison: %s", terminalText(change.Reason))
		}
		for _, key := range change.Changes {
			terminalChange(w, "   ", key)
		}
	}
}

func terminalList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	shown, more := bounded(values, 8)
	result := terminalText(strings.Join(shown, ", "))
	if more > 0 {
		result += fmt.Sprintf(" (+%d more; use JSON)", more)
	}
	return result
}

// terminalRepeated shows identities rendered more than once; like Argo CD's
// RepeatedResourceWarning, the last rendered occurrence is compared.
func terminalRepeated(w *terminalWriter, a domain.ApplicationResult) {
	for _, side := range []struct {
		name     string
		repeated []domain.ResourceID
		target   *domain.TargetSummary
	}{{"base", a.BaseRepeatedResources, a.BaseTarget}, {"head", a.HeadRepeatedResources, a.HeadTarget}} {
		if side.target != nil {
			side.repeated = side.target.RepeatedResources
		}
		if len(side.repeated) == 0 {
			continue
		}
		w.line("  [!] Repeated in %s target: %d resources; the last rendered occurrence is compared (Argo CD RepeatedResourceWarning)", side.name, len(side.repeated))
		for i, id := range side.repeated {
			if i >= 4 {
				w.line("    ... %d more; see JSON", len(side.repeated)-i)
				break
			}
			w.line("    %s", terminalResource(id))
		}
	}
}
