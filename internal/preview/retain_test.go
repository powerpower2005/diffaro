package preview

import (
	"context"
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

// An Application past the retained-byte budget has the same outcome in
// explicit and graph mode: state unknown, execution failed with the limit
// issue, and the root and graph path of a kept result. The run still fails.
func TestRetainedBudgetEntryIsTheSameInBothModes(t *testing.T) {
	runner := graphRunner(graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		return []string{configMap(rev)}, nil
	}}, "root")
	modes := map[string]func(Runner) domain.Report{
		"explicit": func(r Runner) domain.Report { return r.Run(context.Background(), graphPair()) },
		"graph":    func(r Runner) domain.Report { return r.RunRecursive(context.Background(), graphPair()) },
	}
	for name, run := range modes {
		t.Run(name, func(t *testing.T) {
			kept := findApp(t, run(runner), "root")
			bounded := runner
			bounded.maxResultBytes = 1
			report := run(bounded)
			app := findApp(t, report, "root")
			if app.State != domain.StateUnknown || app.Execution != domain.ExecutionFailed || len(app.Issues) != 1 || app.Issues[0].Code != domain.CodeReportLimitExceeded {
				t.Fatalf("state %s execution %s issues %+v", app.State, app.Execution, app.Issues)
			}
			if app.Root != kept.Root || !reflect.DeepEqual(app.GraphPath, kept.GraphPath) || len(app.GraphPath) == 0 {
				t.Fatalf("root %s path %v, want those of the kept result: %s %v", app.Root, app.GraphPath, kept.Root, kept.GraphPath)
			}
			if report.Status != domain.StatusFailed || report.ExitCode() != 6 {
				t.Fatalf("status %s exit %d, want the limit counted as a failure", report.Status, report.ExitCode())
			}
		})
	}
}
