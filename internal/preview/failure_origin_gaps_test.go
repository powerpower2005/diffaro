package preview

import (
	"context"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/reporter"
)

// A backend server error is retryable by its reason code alone, even when the
// issue code itself is not a transient code.
func TestFailureOriginServerErrorReasonIsTransient(t *testing.T) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	issue := domain.Issue{Code: "backend_error", ReasonCode: "server_error", Stage: "connection", Reason: "HTTP 502", Side: "head", Application: &id}
	if transientFailures[issue.Code] {
		t.Fatal("fixture must use a code that is not transient by itself")
	}
	app := domain.ApplicationResult{ID: id, BaseTarget: &domain.TargetSummary{Execution: "complete"}, HeadTarget: &domain.TargetSummary{Execution: "failed"}, Issues: []domain.Issue{issue}}
	if got := failureOrigin(app, true); got != domain.FailureTransient {
		t.Fatalf("origin=%q, want %q for a server error", got, domain.FailureTransient)
	}
	// The same head failure with another reason code is introduced by the PR.
	app.Issues[0].ReasonCode = "unexpected_status"
	if got := failureOrigin(app, true); got != domain.FailureIntroduced {
		t.Fatalf("origin=%q, want %q for a non-retryable reason", got, domain.FailureIntroduced)
	}
}

// An ineligible candidate is still rendered, but the discovered run records
// on its discovery decision that the PR does not change its inputs; that
// proof is what makes its same failure on both revisions preexisting.
func TestDiscoveredIneligibleCandidateRecordsUnchangedInputs(t *testing.T) {
	var mu sync.Mutex
	rendered := map[string]int{}
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		mu.Lock()
		rendered[app.ID.Name]++
		mu.Unlock()
		if app.ID.Name == "unknown" {
			return nil, domain.FailReason("not_found", "chart_path_missing", "render", "chart path charts/unknown does not exist at this revision", "check the source path", nil)
		}
		return []string{configMap(rev)}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	chart := []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n")
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"charts/changed/values.yaml"}, Read: func(_ context.Context, file string) ([]byte, bool, error) {
		return chart, strings.HasSuffix(file, "Chart.yaml"), nil
	}}
	no := false
	inv := &fakeInventory{apps: []domain.OwnedApplication{
		{Application: graphApp("changed"), ManagesApplications: &no},
		// Unknown child status: ineligible for skipping, so it is rendered.
		{Application: graphApp("unknown")},
	}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if rendered["unknown"] == 0 || rendered["changed"] == 0 {
		t.Fatalf("render calls %v", rendered)
	}
	decisions := map[string]domain.DiscoveryDecision{}
	for _, decision := range report.Discovery[0].Decisions {
		decisions[decision.ID.Name] = decision
	}
	if d := decisions["unknown"]; d.Selection != selectionIneligible || !d.InputsUnchanged {
		t.Fatalf("ineligible candidate decision %+v, want ineligible with unchanged inputs", d)
	}
	if d := decisions["changed"]; d.Selection != selectionRendered || d.InputsUnchanged {
		t.Fatalf("touched candidate decision %+v, want rendered with changed inputs", d)
	}
	AnnotateFailureOrigins(&report)
	if got := findApp(t, report, "unknown"); got.Execution != "failed" || got.FailureOrigin != domain.FailurePreexisting {
		t.Fatalf("execution=%s origin=%q, want failed %q", got.Execution, got.FailureOrigin, domain.FailurePreexisting)
	}
	// The failed Application's issues are also listed at report level; the
	// verdict counts the Application once, not those issues as run-level.
	listed := 0
	for _, issue := range report.Issues {
		if issue.Application != nil && issue.Application.Name == "unknown" {
			listed++
		}
	}
	verdict := strings.Join(reporter.VerdictLines(report), "\n")
	if listed == 0 || strings.Contains(verdict, "run-level") || !strings.Contains(verdict, "Counted failures: 1 Application(s) already failing before this PR\n") {
		t.Fatalf("report-level issues=%d, verdict:\n%s", listed, verdict)
	}
}
