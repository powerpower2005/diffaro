package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestVerdictExplainsWhatDecidedTheExitCode(t *testing.T) {
	app := func(name, state, execution, origin string) domain.ApplicationResult {
		return domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}, State: domain.State(state), Execution: domain.Execution(execution), FailureOrigin: domain.FailureOrigin(origin)}
	}
	report := func(status string, excluded int, apps ...domain.ApplicationResult) domain.Report {
		r := domain.NewReport()
		r.Status, r.PreexistingFailuresExcluded, r.Applications = domain.Status(status), excluded, apps
		for _, a := range apps {
			r.ChangesFound = r.ChangesFound || a.State == "changed"
		}
		return r
	}
	changed := app("api", "changed", "complete", "")
	old := app("old", "unknown", "failed", "preexisting")
	broken := app("new", "unknown", "failed", "introduced")
	skipped := app("web", "unknown", "skipped", "")
	skipped.SkipReason = "helper function unavailable"
	longSkip := app("web", "unknown", "skipped", "")
	// Skip reasons put their specific cause after a general explanation.
	longSkip.SkipReason = "Local Helm analysis skipped: " + strings.Repeat("general explanation ", 10) + "template web/templates/role.yaml:16 uses unavailable function \"helperFunc\""
	runIssue := report("partial_failure", 0, changed)
	runIssue.Issues = []domain.Issue{{Code: "selection_changed", Stage: "selection", Reason: "inventory changed"}}
	for _, tc := range []struct {
		name   string
		report domain.Report
		want   []string
		absent []string
	}{
		{"preexisting failures counted while the opt-in is off", report("partial_failure", 0, changed, old),
			[]string{"Verdict: partial failure (exit 3)", "Counted failures: 1 Application(s) already failing before this PR", "it is off, so they count"}, []string{"Not counted"}},
		{"preexisting failures excluded by the opt-in", report("success", 1, changed, old),
			[]string{"Verdict: changes found (exit 1)", "Counted failures: none", "Not counted: 1 Application(s) already failing before this PR (failures.excludePreexistingFromExit)"}, []string{"it is off"}},
		{"a failure caused by the PR always counts", report("partial_failure", 1, changed, old, broken),
			[]string{"Verdict: partial failure (exit 3)", "Counted failures: 1 Application(s) broken by this PR", "Not counted: 1"}, nil},
		{"a failure whose inputs the PR changes is named", report("partial_failure", 0, changed, app("vm", "unknown", "failed", "inputs-changed")),
			[]string{"Counted failures: 1 Application(s) already failing whose inputs this PR changes"}, []string{"it is off"}},
		{"run-level issues are counted", runIssue,
			[]string{"Counted failures: 1 run-level issue(s)"}, []string{"it is off"}},
		{"clean run", report("success", 0, app("api", "unchanged", "complete", "")),
			[]string{"Verdict: no changes (exit 0)", "Counted failures: none", "Changes: 0 Application(s)"}, []string{"Not counted", "Not analyzed"}},
		{"a skipped Application leaves changes unknown", report("success", 0, app("api", "unchanged", "complete", ""), skipped),
			[]string{"Verdict: no changes in analyzed Applications (exit 0)", "Not analyzed: 1 Application(s) skipped, resource changes unknown: east/argocd/web (helper function unavailable)"}, []string{"Verdict: no changes ("}},
		{"a long skip reason keeps its specific cause", report("success", 0, longSkip),
			[]string{"uses unavailable function \"helperFunc\")"}, nil},
		{"changes found with a skipped Application", report("success", 0, changed, skipped),
			[]string{"Verdict: changes found (exit 1)", "Not analyzed: 1 Application(s) skipped"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(verdictLines(tc.report), "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("unexpected %q in\n%s", absent, got)
				}
			}
		})
	}
}

// Every format prints the verdict, so a skipped Application is named in each.
func TestSkippedApplicationsAreNamedInEveryVerdict(t *testing.T) {
	r := domain.NewReport()
	r.Status = "success"
	r.Applications = []domain.ApplicationResult{{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "web"}, State: "unknown", Execution: "skipped", SkipReason: "helper function unavailable"}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"Verdict: no changes in analyzed Applications (exit 0)", "Not analyzed: 1 Application(s) skipped, resource changes unknown: east/argocd/web (helper function unavailable)"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
	}
}

// doctor without --render reads access only. No format states a comparison
// result for it; a failure keeps its status label.
func TestPreflightVerdictStatesNoComparison(t *testing.T) {
	r := domain.NewReport()
	r.Coverage = domain.CoverageConnectionPreflight
	r.Applications = []domain.ApplicationResult{{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, State: domain.StateUnchanged, Execution: domain.ExecutionComplete}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if !strings.Contains(out, "Verdict: read access checked; targets were not rendered (exit 0)") || !strings.Contains(out, "Changes: not compared; run doctor --render --pr NUMBER") || strings.Contains(out, "no changes") || strings.Contains(out, "Changes: 0 Application(s)") {
			t.Fatalf("%s states a comparison result for a preflight:\n%s", format, out)
		}
	}
	r.Status = domain.StatusPartialFailure
	if lines := verdictLines(r); lines[0] != "Verdict: partial failure (exit 3)" {
		t.Fatalf("preflight failure lost its status: %q", lines[0])
	}
}
