package preview

import (
	"context"
	"encoding/json"
	"testing"

	"diffaro/internal/domain"
)

// TestSkippedGraphResultKeepsRunPartial requires the graph modes to count a
// skipped Application as a useful result, as explicit mode and run-level
// issues do: a recursive run whose only kept result is a skipped child and
// whose compared root fell outside the retained-byte budget is a partial
// failure, not an overall failure.
func TestSkippedGraphResultKeepsRunPartial(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"zroot": graphApp("zroot")}, render: func(_ context.Context, a domain.Application, _ string) ([]string, error) {
		if a.ID.Name == "zroot" {
			return []string{appManifest(graphApp("achild"))}, nil
		}
		return nil, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
	}}
	runner := graphRunner(b, "zroot")
	full := runner.RunRecursive(context.Background(), graphPair())
	child := findApp(t, full, "achild")
	if full.Status != domain.StatusSuccess || child.Execution != domain.ExecutionSkipped || len(child.Resources) != 0 || full.Applications[0].ID.Name != "achild" {
		t.Fatalf("fixture must hold a skipped child before a compared root: %s %+v", full.Status, child)
	}
	encoded, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	runner.maxResultBytes = len(encoded)
	report := runner.RunRecursive(context.Background(), graphPair())
	root := findApp(t, report, "zroot")
	if findApp(t, report, "achild").Execution != domain.ExecutionSkipped || root.Execution != domain.ExecutionFailed || root.Issues[0].Code != domain.CodeReportLimitExceeded {
		t.Fatal("the budget must keep only the skipped child")
	}
	if report.Status != domain.StatusPartialFailure || report.ExitCode() != 3 {
		t.Fatalf("a kept skipped result must make the run a partial failure: %s exit %d", report.Status, report.ExitCode())
	}
}
