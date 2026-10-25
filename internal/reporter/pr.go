package reporter

import (
	"bytes"
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

// MaxPRMarkdownBytes bounds the standalone Markdown artifact for external PR
// integrations. The full JSON remains the machine record.
const MaxPRMarkdownBytes = 60 << 10

// The marker lets an external integration find its previous comment. This
// package only produces bytes; it never reads or writes a hosting provider.
const prMarker = "<!-- kubeseer:preview -->\n"

// WithoutIntegrationMarkers removes the leading marker and Base/Head comment
// of encoded PR Markdown, so a preview, such as the offline demo's, cannot be
// taken for a PR result by an integration.
func WithoutIntegrationMarkers(data []byte) []byte {
	text := strings.TrimPrefix(string(data), prMarker)
	if strings.HasPrefix(text, "<!-- Base: ") {
		if end := strings.Index(text, " -->\n"); end >= 0 {
			text = text[end+len(" -->\n"):]
		}
	}
	return []byte(text)
}

func encodePR(v *reportView) []byte {
	report := v.report
	var b strings.Builder
	b.WriteString(prMarker)
	// Keep the integration identity even if a large graph exhausts the byte budget.
	fmt.Fprintf(&b, "<!-- Base: %s Head: %s -->\n", prText(report.Revisions.Base, 80), prText(report.Revisions.Head, 80))
	writePRHeadline(&b, v)
	writePRErrors(&b, v)
	// Coverage warnings; coverage says how they differ from the other formats.
	if v.coverage.skipped > 0 {
		b.WriteString("**Some target resources and descendants were not analyzed.** Zero reported resource changes in a skipped branch do not mean no changes.\n\n")
	}
	if v.coverage.statusIncomplete && len(v.errors) == 0 {
		// Needs attention states the failures when there are any.
		b.WriteString("**Comparison is incomplete or denied.** Successful branches remain available.\n\n")
	}
	if v.coverage.graphIncomplete {
		b.WriteString("**Graph discovery is incomplete.** Missing branches are not evidence of removal.\n\n")
	}
	if report.Policy != nil {
		fmt.Fprintf(&b, "**Policy evaluation:** `%s` · %d violation(s)\n\n", prText(report.Policy.Evaluation, 40), len(report.Policy.Violations))
	}
	for i, violation := range report.PolicyViolations {
		if i == 12 {
			b.WriteString("- More policy violations are in JSON.\n")
			break
		}
		fmt.Fprintf(&b, "- **Policy:** %s\n", prText(violation, 180))
	}
	if len(report.PolicyViolations) > 0 {
		b.WriteString("\n")
	}
	writePRChanges(&b, v.prGroups)
	writeInputSummary(&b, v)
	b.WriteString("<details>\n<summary>How this was compared: revisions, Application selection, render context and limits</summary>\n\n")
	writePREvidence(&b, v)
	b.WriteString("\n</details>\n")
	return boundPR([]byte(htmlCodeSpans(b.String())))
}

// writePREvidence writes the collapsed comparison details: status,
// revisions, verdict, policies, renderers, Application selection, the
// Applications, graph links, render context and limits.
func writePREvidence(b *strings.Builder, v *reportView) {
	report := v.report
	fmt.Fprintf(b, "Status: `%s` · Coverage: `%s` · Redaction: `%s`\n\n", prText(string(report.Status), 60), prText(report.Coverage, 80), prText(report.Redaction, 60))
	fmt.Fprintf(b, "Repository: `%s` · base: `%s` · head: `%s`\n\n", prText(report.Revisions.Repository, 200), prText(report.Revisions.Base, 80), prText(report.Revisions.Head, 80))
	writeVerdict(b, v.verdict)
	writePolicy(b, report, true)
	for _, backend := range report.RenderBackends {
		fmt.Fprintf(b, "Renderer `%s`: `%s` · context `%s` · verification `%s`\n\n", prText(backend.Instance, 120), prText(backend.Kind, 80), prText(backend.ContextID, 120), prText(backend.Verification, 60))
		if line := backendVersionLine(prText(backend.Version, 80), prText(backend.Compatibility, 80)); line != "" {
			b.WriteString(line)
		}
	}
	writeSelections(b, report, "#### Application selection")
	if len(v.prApps) > 0 {
		b.WriteString("#### Applications\n\n| Application | State | Execution | Resources + / ~ / − |\n|---|---|---|---|\n")
	}
	for i, app := range v.prApps {
		if i == 30 {
			b.WriteString("\nMore Applications are in JSON.\n")
			break
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", prText(app.id, 180), prText(string(app.State), 40), prText(string(app.Execution), 40), stateCounts(app.resources, changeStates, false, " / "))
	}
	if report.Graph != nil && len(report.Graph.Changes) > 0 {
		b.WriteString("\nGraph links (shared children retain every parent):\n\n")
		for i, edge := range report.Graph.Changes {
			if i == 30 {
				b.WriteString("- More graph links are in JSON.\n")
				break
			}
			fmt.Fprintf(b, "- %s: `%s` → `%s`\n", prText(string(edge.State), 40), prText(edge.Parent.String(), 180), prText(edge.Child.String(), 180))
		}
	}
	if len(v.prContext) > 0 {
		b.WriteString("\n#### Render context\n\n")
	}
	// Change sections come first and share their bound, so each shown
	// section's render context link has its anchor.
	for i, app := range v.prContext {
		if i == prMaxAppSections {
			b.WriteString("More Application context is in JSON.\n")
			break
		}
		fmt.Fprintf(b, "<a id=\"%s\"></a>\n\n**%s**\n\nDestination: `%s` · Project: `%s` · Namespace: `%s`\n\n", appAnchor(app.ID), prText(app.id, 180), prText(app.destination.label(), 180), prText(app.Project, 80), prText(app.Namespace, 80))
		writePRTargetContext(b, app.ApplicationResult)
		writeSourceContext(b, app.ApplicationResult, true)
	}
	if len(report.Limitations) > 0 {
		b.WriteString("\nComparison limits: ")
		for i, item := range report.Limitations {
			if i >= 2 {
				break
			}
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(prText(item, 180))
			if !strings.HasSuffix(item, ".") {
				b.WriteString(".")
			}
		}
		b.WriteString("\n")
	}
}

func writePRTargetContext(b *strings.Builder, a domain.ApplicationResult) {
	root := a.Root.String()
	if a.Root.Name == "" {
		root = "unknown"
	}
	fmt.Fprintf(b, "Root: `%s`\n\n", prText(root, 180))
	pathText := func(ids []domain.ApplicationID) string {
		if len(ids) == 0 {
			return "unknown"
		}
		if len(ids) > 6 {
			return domain.JoinGraphPath(ids[:6], " -> ") + " -> more path entries in JSON"
		}
		return domain.JoinGraphPath(ids, " -> ")
	}
	fmt.Fprintf(b, "Discovery path: `%s`. Shared incoming links are in graph evidence/JSON.\n\n", prText(pathText(a.GraphPath), 320))
	for _, side := range []struct {
		name   string
		target *domain.TargetSummary
	}{{"Base", a.BaseTarget}, {"Head", a.HeadTarget}} {
		if side.target == nil {
			if (side.name == "Base" && a.State == domain.StateAdded) || (side.name == "Head" && a.State == domain.StateRemoved) {
				fmt.Fprintf(b, "%s target: absent (%s Application).\n\n", side.name, prText(string(a.State), 40))
			}
			continue
		}
		t := side.target
		fmt.Fprintf(b, "%s target: `%s` · `%s` @ `%s`%s · destination `%s` / `%s`\n\n", side.name, prText(string(t.Execution), 40), prText(t.Repository, 160), prText(t.Revision, 80), resolvedSuffix(t.RequestedRevision, func(s string) string { return "`" + prText(s, 120) + "`" }), prText(knownDestination(t.Cluster), 80), prText(knownDestination(t.Namespace), 80))
		fmt.Fprintf(b, "Input: `%s` · release `%s` · origin `%s` · path `%s`\n\n", prText(t.Inputs.Path, 100), prText(t.Inputs.ReleaseName, 80), prText(t.Inputs.Origin, 80), prText(pathText(t.GraphPath), 240))
	}
}

func writePRAccess(b *strings.Builder, access *domain.AccessRequirement) {
	if access == nil {
		return
	}
	fmt.Fprintf(b, "- Required access: %s.", strings.Join(accessFields(access, prText), ", "))
	if access.Object != "" {
		fmt.Fprintf(b, " RBAC object: `%s`.", prText(access.Object, 240))
	}
	if access.Scope != "" {
		fmt.Fprintf(b, " Scope: %s", prText(access.Scope, 360))
	}
	fmt.Fprintln(b)
}

func boundPR(data []byte) []byte {
	if len(data) <= MaxPRMarkdownBytes {
		return data
	}
	const notice = "\n\n**Comment shortened to 60 KiB.** Inspect JSON or the full Markdown report for omitted details.\n"
	// A text diff fence is at most one tilde longer than a bounded line.
	limit := MaxPRMarkdownBytes - len(notice) - len("\n</details>\n") - 256
	cut := bytes.LastIndexByte(data[:limit], '\n')
	if cut < 0 {
		cut = 0
	}
	result := append([]byte{}, data[:cut]...)
	fence := ""
	for _, line := range strings.SplitAfter(string(result), "\n") {
		fence, _ = textFence(fence, line)
	}
	if fence != "" {
		result = append(result, "\n"+fence+"\n"...)
	}
	if bytes.Count(result, []byte("<details>")) > bytes.Count(result, []byte("</details>")) {
		result = append(result, "\n</details>\n"...)
	}
	return append(result, notice...)
}
