package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// The terminal report leaves out a label whose value is empty, as console
// diagnostics do, and the verdict counts a run-level cause at base and head
// once, as the Issues sections group it.
func TestTerminalOmitsEmptyLabelsAndVerdictCountsGroupedIssues(t *testing.T) {
	failed := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	root := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "root"}
	r := domain.NewReport()
	r.Status = domain.StatusFailed
	r.Applications = []domain.ApplicationResult{{ID: failed, State: domain.StateFailed, Execution: domain.ExecutionFailed, Issues: []domain.Issue{
		{Code: domain.CodeLocalRenderFailed, ReasonCode: domain.ReasonTemplateFailed, Stage: "render", Reason: "template failed", Action: "fix the chart", Application: &failed},
	}}}
	cycle := domain.Issue{Code: domain.CodeGraphCycle, Stage: "graph", Reason: "Application identity repeats on the active graph path", Action: "remove the cycle", Application: &root}
	base, head := cycle, cycle
	base.Side, base.Revision, head.Side, head.Revision = "base", "aaaa", "head", "bbbb"
	r.Issues = []domain.Issue{base, head}
	data, err := Encode(r, "terminal", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"[!] local_render_failed | Stage: render | Cause: template_failed", "[!] graph_cycle | Stage: graph | Side: base, head\n", "1 run-level issue(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("terminal lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Stage:") && (strings.HasSuffix(line, ": ") || strings.Contains(line, ": |") || strings.Contains(line, ":  ")) {
			t.Fatalf("empty label in %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "2 run-level issue(s)") {
		t.Fatalf("one cause at base and head counted twice:\n%s", out)
	}
}
