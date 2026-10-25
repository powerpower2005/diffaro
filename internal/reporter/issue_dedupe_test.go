package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Issues that differ only by repository or stage require different fixes, so
// every human-readable format must keep them while collapsing exact repeats.
func TestIssueDedupeKeepsRepositoryAndStageDifferences(t *testing.T) {
	id := domain.ApplicationID{Instance: "lab", Namespace: "argocd", Name: "api"}
	issue := domain.Issue{Code: "render_failed", Stage: "render", Reason: "values unavailable", Action: "check values", Repository: "example/charts", Revision: strings.Repeat("a", 40), Application: &id}
	otherRepository, otherStage := issue, issue
	otherRepository.Repository = "example/values"
	otherStage.Stage = "discovery"
	r := domain.NewReport()
	r.Status = "partial_failure"
	r.Issues = []domain.Issue{issue, otherRepository, otherStage, issue}
	r.Applications = []domain.ApplicationResult{{ID: id, State: "unknown", Execution: "failed", Issues: []domain.Issue{issue, otherRepository}}}

	if got := collectIssues(r); len(got) != 3 {
		t.Fatalf("PR issues = %d, want 3: %+v", len(got), got)
	}
	terminal, err := Encode(r, "terminal", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	out := string(terminal)
	if !strings.Contains(out, "Issues: 3") || strings.Count(out, "[!] render_failed") != 3 || !strings.Contains(out, "example/values @") || !strings.Contains(out, "Stage: discovery") {
		t.Fatalf("terminal collapsed distinct issues or kept duplicates\n%s", out)
	}
}
