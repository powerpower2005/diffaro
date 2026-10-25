package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
)

// sideFailures renders a root with a per-revision error; a nil or partial
// error renders a ConfigMap.
func sideFailures(base, head error) graphBackend {
	return graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		err := base
		if rev == graphHead {
			err = head
		}
		if err != nil && !domain.IsRenderPartial(err) {
			return nil, err
		}
		return []string{configMap(rev)}, err
	}}
}

func deniedRender() error {
	return domain.Fail(domain.CodeAccessDenied, "render", "permission denied", "grant read access", nil)
}

func failedRender() error {
	return domain.Fail(domain.CodeRenderFailed, "render", "template failed", "fix the chart", nil)
}

func partialRender() error {
	return domain.PartialRender([]domain.Issue{{Code: domain.CodeRenderPartial, Stage: "render", Reason: "incomplete Helm targets"}})
}

// issueSides maps each side named by an Application's issues to its code.
func issueSides(app domain.ApplicationResult) map[string]string {
	out := map[string]string{}
	for _, issue := range app.Issues {
		out[issue.Side] = issue.Code
	}
	return out
}

// A graph node failing on both revisions is denied only when both sides are
// denied, whichever side is denied.
func TestGraphTwoSidedFailureExecutionIgnoresSideOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, head error
		want       domain.Execution
	}{
		{"base denied, head render failed", deniedRender(), failedRender(), domain.ExecutionFailed},
		{"base render failed, head denied", failedRender(), deniedRender(), domain.ExecutionFailed},
		{"both denied", deniedRender(), deniedRender(), domain.ExecutionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := graphRunner(sideFailures(tc.base, tc.head), "root").RunRecursive(context.Background(), graphPair())
			root := findApp(t, report, "root")
			sides := issueSides(root)
			if sides["base"] != issueCode(tc.base) || sides["head"] != issueCode(tc.head) {
				t.Fatalf("both side failures must be reported: %+v", root.Issues)
			}
			if root.Execution != tc.want {
				t.Fatalf("execution %s, want %s", root.Execution, tc.want)
			}
		})
	}
}

// An explicit Application whose base renders partially and whose head then
// fails keeps both failing sides. A partial render is never a denial, so the
// Application is failed, as a graph node with the same sides is.
func TestExplicitPartialBaseAndFailedHeadKeepBothSides(t *testing.T) {
	for _, tc := range []struct {
		name string
		head error
	}{
		{"head denied", deniedRender()},
		{"head render failed", failedRender()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := graphRunner(sideFailures(partialRender(), tc.head), "root")
			explicit := findApp(t, runner.Run(context.Background(), graphPair()), "root")
			head := 0
			for _, issue := range explicit.Issues {
				if issue.Revision == graphHead && issue.Code == issueCode(tc.head) {
					head++
				}
			}
			if issueSides(explicit)["base"] != domain.CodeRenderPartial || head != 1 || len(explicit.Issues) != 2 {
				t.Fatalf("the partial base failure must stay next to the head failure: %+v", explicit.Issues)
			}
			recursive := findApp(t, runner.RunRecursive(context.Background(), graphPair()), "root")
			if explicit.Execution != domain.ExecutionFailed || explicit.State != domain.StateFailed || recursive.Execution != explicit.Execution {
				t.Fatalf("explicit execution %s state %s, recursive execution %s; want failed in both modes", explicit.Execution, explicit.State, recursive.Execution)
			}
		})
	}
}

// An explicit base render that fails outright discards the head side, so
// the base failure alone decides the execution.
func TestExplicitOutrightBaseFailureDecidesAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, head error
		want       domain.Execution
	}{
		{"base denied, head render failed", deniedRender(), failedRender(), domain.ExecutionDenied},
		{"base render failed, head denied", failedRender(), deniedRender(), domain.ExecutionFailed},
		{"base denied, head partial", deniedRender(), partialRender(), domain.ExecutionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := graphRunner(sideFailures(tc.base, tc.head), "root").Run(context.Background(), graphPair())
			root := findApp(t, report, "root")
			if len(root.Issues) != 1 || root.Issues[0].Revision != graphBase || root.Execution != tc.want {
				t.Fatalf("execution %s issues %+v, want %s from the base side only", root.Execution, root.Issues, tc.want)
			}
		})
	}
}

// An explicit Application whose base renders partially and whose head is
// skipped stays failed with the base issue, as a graph node with the same
// sides does: the skipped head does not hide the base failure.
func TestExplicitPartialBaseAndSkippedHeadKeepTheFailure(t *testing.T) {
	runner := graphRunner(sideFailures(partialRender(), &domain.RenderSkipped{Reason: "head render context unavailable"}), "root")
	report := runner.Run(context.Background(), graphPair())
	explicit := findApp(t, report, "root")
	recursive := findApp(t, runner.RunRecursive(context.Background(), graphPair()), "root")
	if issueSides(explicit)["base"] != domain.CodeRenderPartial || len(explicit.Issues) != len(recursive.Issues) {
		t.Fatalf("the partial base failure must stay reported: explicit %+v, recursive %+v", explicit.Issues, recursive.Issues)
	}
	if explicit.Execution != domain.ExecutionFailed || explicit.Execution != recursive.Execution || explicit.State != recursive.State {
		t.Fatalf("explicit execution %s state %s, recursive execution %s state %s; want the same failed result", explicit.Execution, explicit.State, recursive.Execution, recursive.State)
	}
	if report.Status != domain.StatusFailed {
		t.Fatalf("status %s, want failed", report.Status)
	}
}
