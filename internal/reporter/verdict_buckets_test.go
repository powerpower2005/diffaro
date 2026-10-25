package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func verdictText(r domain.Report) string { return strings.Join(verdictLines(r), "\n") }

// Each counted failure is named by its bucket, the change count is the number
// of added, changed and removed Applications, and the rules pointer is
// always present.
func TestVerdictBucketsAndCounts(t *testing.T) {
	changed := func(name, state string) func() (domain.ApplicationResult, []domain.Issue) {
		return func() (domain.ApplicationResult, []domain.Issue) {
			return domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}, State: domain.State(state), Execution: "complete"}, nil
		}
	}
	failed := func(name, origin, base string) func() (domain.ApplicationResult, []domain.Issue) {
		return func() (domain.ApplicationResult, []domain.Issue) {
			return originFailure(name, origin, "connection_failed", "", base, "failed")
		}
	}
	denied := func() (domain.ApplicationResult, []domain.Issue) {
		app, issues := originFailure("locked", "", "access_denied", "request_forbidden", "failed", "failed")
		app.Execution = "denied"
		return app, issues
	}
	for _, tc := range []struct {
		name   string
		report domain.Report
		want   []string
		absent []string
	}{
		{"transient", failureReport(changed("api", "changed"), failed("flaky", "transient", "complete")),
			[]string{"Counted failures: 1 Application(s) that may pass on retry"}, []string{"other causes", "run-level"}},
		{"denied", failureReport(changed("api", "changed"), denied),
			[]string{"Counted failures: 1 Application(s) denied access"}, []string{"other causes", "run-level"}},
		{"changes", failureReport(changed("api", "changed"), changed("web", "added"), changed("gone", "removed"), changed("same", "unchanged")),
			[]string{"- Changes: 3 Application(s) added, changed or removed"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := verdictText(tc.report)
			for _, want := range append(tc.want, "- Rules: see Exit codes in the Diffaro reference (docs/reference.md#exit-codes)") {
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

// Preview lists a failed Application's issues on the Application and at
// report level. The verdict counts that Application once, not its issues
// again as run-level issues; only issues of no failed Application are.
func TestVerdictDoesNotCountFailedApplicationIssuesAsRunLevel(t *testing.T) {
	r := failureReport(func() (domain.ApplicationResult, []domain.Issue) {
		return originFailure("old", "preexisting", "not_found", "chart_path_missing", "failed", "failed")
	})
	if len(r.Issues) != 2 {
		t.Fatalf("fixture must list both sides' issues at report level: %d", len(r.Issues))
	}
	got := verdictText(r)
	if !strings.Contains(got, "Counted failures: 1 Application(s) already failing before this PR\n") || strings.Contains(got, "run-level") {
		t.Fatalf("failed Application issues counted again:\n%s", got)
	}
	r.Issues = append(r.Issues, domain.Issue{Code: "selection_changed", Stage: "selection", Reason: "inventory changed"})
	if got := verdictText(r); !strings.Contains(got, "1 run-level issue(s)") {
		t.Fatalf("an issue of no failed Application is not counted:\n%s", got)
	}
}

// The opt-in hint appears only when every counted failure predates the PR;
// another failure or a run-level issue beside a preexisting one removes it.
func TestVerdictPreexistingHintOnlyWhenEveryFailurePredates(t *testing.T) {
	const hint = "Every counted failure predates this PR"
	preexisting := func() (domain.ApplicationResult, []domain.Issue) {
		return originFailure("old", "preexisting", "not_found", "chart_path_missing", "failed", "failed")
	}
	introduced := func() (domain.ApplicationResult, []domain.Issue) {
		return originFailure("new", "introduced", "not_found", "chart_path_missing", "complete", "failed")
	}
	runIssue := failureReport(preexisting)
	runIssue.Issues = append(runIssue.Issues, domain.Issue{Code: "selection_changed", Stage: "selection", Reason: "inventory changed"})
	for _, tc := range []struct {
		name   string
		report domain.Report
		hint   bool
	}{
		{"only preexisting failures", failureReport(preexisting), true},
		{"a preexisting and an introduced failure", failureReport(preexisting, introduced), false},
		{"a preexisting failure and a run-level issue", runIssue, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := verdictText(tc.report)
			if strings.Contains(got, hint) != tc.hint {
				t.Fatalf("hint present=%t, want %t:\n%s", !tc.hint, tc.hint, got)
			}
		})
	}
}
