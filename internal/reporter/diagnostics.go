package reporter

import (
	"fmt"
	"io"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// WriteIssue writes one issue for stderr: redacted, one fact per line and
// every line bounded, with the required access, the internal_error site and
// cause chain, a panic stack, and the action split into steps.
func WriteIssue(out io.Writer, issue domain.Issue, redactor security.Redactor) {
	text := func(value string) string { return singleLine(value, redactor) }
	var message strings.Builder
	fmt.Fprintf(&message, "%s [stage=%s]: %s\n", text(issue.Code), text(issue.Stage), text(issue.Reason))
	if issue.ReasonCode != "" && issue.Code == domain.CodeAccessDenied {
		fmt.Fprintf(&message, "  Cause: %s (%s)\n", text(issue.ReasonCode), text(accessCauseDescription(issue.ReasonCode)))
	} else if issue.ReasonCode != "" {
		fmt.Fprintf(&message, "  Cause: %s\n", text(issue.ReasonCode))
	}
	if id := issue.Application; id != nil && id.Name == "" {
		// A run-level failure of one instance names no Application.
		fmt.Fprintf(&message, "  Argo CD instance: %s\n", text(id.Instance))
	} else if id != nil {
		fmt.Fprintf(&message, "  Application: %s\n", text(id.String()))
	}
	if issue.SourcePosition > 0 {
		fmt.Fprintf(&message, "  Source position: %d\n", issue.SourcePosition)
	}
	if access := issue.RequiredAccess; access != nil {
		if access.CredentialEnv != "" {
			fmt.Fprintf(&message, "  Credential environment: %s\n", text(access.CredentialEnv))
		} else {
			message.WriteString("  Credential: token from the profile or a credential flag\n")
		}
		fmt.Fprintf(&message, "  Required access: system=%s; instance=%s", text(access.System), text(access.Instance))
		if access.Resource != "" {
			fmt.Fprintf(&message, "; resource=%s", text(access.Resource))
		}
		fmt.Fprintf(&message, "; action=%s\n", text(access.Action))
		fmt.Fprintf(&message, "  Access target: %s\n", text(access.Target))
		if access.Object != "" {
			fmt.Fprintf(&message, "  RBAC object: %s\n", text(access.Object))
		}
		if access.Scope != "" {
			fmt.Fprintf(&message, "  Access scope: %s\n", text(access.Scope))
		}
	}
	for _, field := range []struct{ label, value string }{{"Repository", issue.Repository}, {"Revision", issue.Revision}, {"Side", issue.Side}} {
		if field.value != "" {
			fmt.Fprintf(&message, "  %s: %s\n", field.label, text(field.value))
		}
	}
	action := issue.Action
	if issue.Code == domain.CodeAccessDenied && strings.TrimSpace(action) == "" {
		action = "check the token environment variable and the failed request's read-only permission; if the cause is unknown, inspect the configured gateway and render access binding"
	}
	// The first step is the likely fix; further checks follow one per line.
	steps := strings.Split(action, "; ")
	fmt.Fprintf(&message, "  Required action: %s\n", text(steps[0]))
	for _, step := range steps[1:] {
		fmt.Fprintf(&message, "    - %s\n", text(step))
	}
	if d := issue.Diagnostic; d != nil && issue.Code == domain.CodeInternalError {
		if d.Site != "" {
			fmt.Fprintf(&message, "  Site: %s\n", text(d.Site))
		}
		if len(d.Chain) > 0 {
			fmt.Fprintf(&message, "  Cause chain: %s\n", text(strings.Join(d.Chain, " > ")))
		}
	}
	if issue.Diagnostic != nil && len(issue.Diagnostic.Stack) > 0 {
		message.WriteString("  Stack:\n")
		for _, frame := range issue.Diagnostic.Stack {
			fmt.Fprintf(&message, "    %s\n", text(frame))
		}
	}
	// Keep each diagnostic on bounded, printable lines even for untrusted IDs.
	for _, line := range strings.Split(strings.TrimSuffix(message.String(), "\n"), "\n") {
		runes := []rune(line)
		if len(runes) > 1200 {
			line = string(runes[:1200]) + "… [truncated; inspect the report]"
		}
		fmt.Fprintln(out, line)
	}
}

func accessCauseDescription(code string) string {
	switch code {
	case domain.ReasonCredentialMissing:
		return "no credential was available to this process"
	case domain.ReasonAuthenticationRejected:
		return "the server rejected authentication; verify the supplied credential and endpoint"
	case domain.ReasonRequestForbidden:
		return "the server refused this request; check read permission and gateway policy"
	default:
		return "cause not classified; follow the request-specific action"
	}
}

// firstSide returns the first side of a merged issue's side list.
func firstSide(sides string) string {
	side, _, _ := strings.Cut(sides, ", ")
	return side
}

// WriteDeniedSummary writes the stderr summary of a run denied access.
func WriteDeniedSummary(out io.Writer) {
	fmt.Fprintln(out, "Diffaro: access denied (exit 5). No comparison completed successfully. Fix the cause below and rerun the same command.")
}

// WriteDiagnostics writes a report's stderr diagnostics: skipped targets, the
// status summary and verdict, then up to 5 grouped issues (200 when verbose)
// with their failure origin and, when verbose, the comparison and Application
// context. The caller bounds the bytes written to out.
func WriteDiagnostics(out io.Writer, report domain.Report, redactor security.Redactor, verbose bool) {
	if verbose {
		writeComparisonContext(out, report, redactor)
	}
	writeSkippedTargets(out, report, redactor, verbose)
	// Successful comparisons may have informational issues (for example masked
	// Secret equality). Verbose diagnostics also expose these safe boundaries.
	if report.Status == domain.StatusSuccess && !verbose && report.PreexistingFailuresExcluded == 0 {
		return
	}
	if report.Status == domain.StatusDenied {
		WriteDeniedSummary(out)
	} else if report.Status == domain.StatusPartialFailure {
		fmt.Fprintln(out, "Diffaro: partial failure (exit 3). Available results were preserved; fix the failed branches below.")
	}
	// The verdict says what decided the exit code, including failures left
	// out of it.
	for i, line := range VerdictLines(report) {
		if i == 0 {
			fmt.Fprintln(out, "Diffaro "+redactor.Text(line))
		} else {
			fmt.Fprintln(out, "  "+redactor.Text(strings.TrimPrefix(line, "- ")))
		}
	}
	issues := append([]domain.Issue(nil), report.Issues...)
	for _, app := range report.Applications {
		issues = append(issues, app.Issues...)
	}
	// One failure at base and head prints once with both sides.
	issues = domain.GroupIssues(issues)
	shown := 0
	limit := 5
	if verbose {
		limit = maxDiagnosticRecords
	}
	origins := map[domain.ApplicationID]domain.FailureOrigin{}
	for _, app := range report.Applications {
		origins[app.ID] = app.FailureOrigin
	}
	for _, issue := range issues {
		if shown == limit {
			break
		}
		WriteIssue(out, issue, redactor)
		if issue.Application != nil {
			if label := FailureOriginLabel(origins[*issue.Application]); label != "" {
				fmt.Fprintf(out, "  Failure origin: %s\n", label)
			}
		}
		if verbose && issue.Application != nil {
			for _, app := range report.Applications {
				if app.ID == *issue.Application {
					writeApplicationContext(out, report, app, firstSide(issue.Side), redactor)
					break
				}
			}
		}
		shown++
	}
	if shown == 0 && report.Status == domain.StatusDenied {
		WriteIssue(out, domain.Issue{Code: domain.CodeAccessDenied, Stage: "permission", Reason: "no request-specific cause was recorded; authentication or read access could not be confirmed"}, redactor)
	}
	if len(issues) > shown {
		fmt.Fprintf(out, "%d more issue(s); inspect the Markdown/JSON report.\n", len(issues)-shown)
	}
}

const maxDiagnosticRecords = 200

var lineBreaks = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// singleLine redacts a value and replaces line breaks and tabs with spaces so
// it cannot start a new diagnostic line.
func singleLine(value string, redactor security.Redactor) string {
	return lineBreaks.Replace(redactor.Text(value))
}

// diagnosticText is singleLine bounded to 1100 runes; WriteIssue bounds whole
// lines at 1200 instead.
func diagnosticText(value string, redactor security.Redactor) string {
	value = singleLine(value, redactor)
	runes := []rune(value)
	if len(runes) > 1100 {
		return string(runes[:1100]) + "… [truncated; inspect JSON]"
	}
	return value
}

func writeComparisonContext(out io.Writer, report domain.Report, redactor security.Redactor) {
	for _, entry := range []struct{ name, value string }{
		{"Status", string(report.Status)}, {"Repository", report.Revisions.Repository},
		{"Base", report.Revisions.Base}, {"Head", report.Revisions.Head}, {"Coverage", report.Coverage},
	} {
		fmt.Fprintf(out, "Diffaro diagnostic %s: %s\n", entry.name, diagnosticText(entry.value, redactor))
	}
	if report.Graph != nil {
		fmt.Fprintf(out, "Diffaro diagnostic graph: baseComplete=%t; headComplete=%t\n", report.Graph.BaseComplete, report.Graph.HeadComplete)
	}
}

func writeApplicationContext(out io.Writer, report domain.Report, app domain.ApplicationResult, side string, redactor security.Redactor) {
	root, path, project, cluster, namespace := app.Root, app.GraphPath, app.Project, app.Cluster, app.Namespace
	var target *domain.TargetSummary
	switch side {
	case "base":
		target = app.BaseTarget
	case "head":
		target = app.HeadTarget
	}
	if target != nil {
		root, path, project, cluster, namespace = target.Root, target.GraphPath, target.Project, target.Cluster, target.Namespace
	}
	fields := []struct{ name, value string }{
		{"Argo CD instance", app.ID.Instance}, {"Application state", string(app.State)},
		{"Root", root.String()}, {"Graph path", domain.JoinGraphPath(path, " -> ")},
		{"Project", project}, {"Destination cluster", cluster}, {"Destination namespace", namespace},
	}
	for _, field := range fields {
		if field.value != "" && field.value != "//" {
			fmt.Fprintf(out, "  %s: %s\n", field.name, diagnosticText(field.value, redactor))
		}
	}
	for _, backend := range report.RenderBackends {
		if backend.Instance == app.ID.Instance {
			fmt.Fprintf(out, "  Render backend: %s\n", diagnosticText(backend.Kind, redactor))
			fmt.Fprintf(out, "  Render context: %s\n", diagnosticText(backend.ContextID, redactor))
			if backend.Version != "" {
				fmt.Fprintf(out, "  Backend version: %s\n", diagnosticText(backend.Version, redactor))
			}
			break
		}
	}
}

func writeSkippedTargets(out io.Writer, report domain.Report, redactor security.Redactor, verbose bool) {
	count := 0
	for _, app := range report.Applications {
		if app.Execution == domain.ExecutionSkipped {
			count++
		}
	}
	if count == 0 {
		return
	}
	fmt.Fprintf(out, "Diffaro: %d skipped Application(s). Resource analysis or graph discovery is incomplete; skipped branches are not evidence of no changes.\n", count)
	limit, shown, records := 5, 0, 0
	if verbose {
		limit = maxDiagnosticRecords
	}
	for _, app := range report.Applications {
		if app.Execution != domain.ExecutionSkipped {
			continue
		}
		add := func(side, repository, revision, reason string) {
			records++
			if shown >= limit {
				return
			}
			id := app.ID
			if reason == "" {
				reason = "Analysis was skipped without a recorded reason; coverage cannot be confirmed."
			}
			issue := domain.Issue{Code: domain.CodeAnalysisSkipped, Stage: "render/discovery", Reason: reason,
				Action:      "review the recorded selection and analysis boundary; select --local-helm or --helm-binary PATH for supported Git Helm children, or configure this instance's trusted repoServer backend",
				Application: &id, Side: side, Repository: repository, Revision: revision}
			for _, backend := range report.RenderBackends {
				if backend.Instance == id.Instance && backend.Kind == domain.BackendLocalHelm {
					issue.Action = "review the recorded local Helm boundary; provide readable Git files, dependency inputs and API render context, then retry; missing targets remain unknown"
				}
			}
			WriteIssue(out, issue, redactor)
			if verbose {
				writeApplicationContext(out, report, app, side, redactor)
			}
			shown++
		}
		hasTarget := false
		for _, entry := range []struct {
			side   string
			target *domain.TargetSummary
		}{{"base", app.BaseTarget}, {"head", app.HeadTarget}} {
			if entry.target != nil && entry.target.Execution == domain.ExecutionSkipped {
				hasTarget = true
				reason := entry.target.SkipReason
				if reason == "" {
					reason = app.SkipReason
				}
				add(entry.side, entry.target.Repository, entry.target.Revision, reason)
			}
		}
		if !hasTarget {
			// Opposite-graph uncertainty may skip an otherwise rendered target.
			// Do not attribute that uncertainty to a fabricated failing revision.
			add("", "", "", app.SkipReason)
		}
	}
	if records > shown {
		fmt.Fprintf(out, "%d more skipped target(s); use --verbose or inspect the full JSON report.\n", records-shown)
	}
}
