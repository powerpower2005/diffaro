package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

func TestLargeTargetsPreserveFullComparisonUnderDefaultLimits(t *testing.T) {
	const count = 1050
	base, head := make([]string, count), make([]string, count)
	payload := strings.Repeat("x", 17<<10)
	for i := range base {
		base[i] = fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"config-%04d"},"data":{"setting":"a%s"}}`, i, payload)
		head[i] = fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"config-%04d"},"data":{"setting":"b%s"}}`, i, payload)
	}
	backend := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, _ domain.Application, revision string) ([]string, error) {
		if revision == graphBase {
			return base, nil
		}
		return head, nil
	}}
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive=%t", recursive), func(t *testing.T) {
			runner := graphRunner(backend, "root")
			runner.Config.Render = profile.DefaultRender()
			// This exercises byte/count retention, not wall-clock performance.
			// Race instrumentation and slower CI hosts need a separate time budget.
			runner.Config.Render.Timeout = "5m"
			var report domain.Report
			if recursive {
				report = runner.RunRecursive(context.Background(), graphPair())
			} else {
				report = runner.Run(context.Background(), graphPair())
			}
			if report.Status != "success" || report.ExitCode() != 1 || len(report.Applications) != 1 || len(report.Applications[0].Resources) != count {
				for _, app := range report.Applications {
					t.Logf("Application %s execution=%s issues=%v", app.ID.String(), app.Execution, app.Issues)
				}
				t.Fatalf("large target comparison was truncated: status=%s issues=%v", report.Status, report.Issues)
			}
			if recursive && (!report.Graph.BaseComplete || !report.Graph.HeadComplete) {
				t.Fatal("graph retention rejected an accepted target response")
			}
			for _, resource := range report.Applications[0].Resources {
				if resource.State != "changed" || resource.Comparison != "complete" || len(resource.Changes) != 1 || resource.Changes[0].Path != "/data/setting" {
					t.Fatal("large resource changes were lost")
				}
			}
			encoded, err := reporter.Encode(report, "json", security.Redactor{})
			if err != nil || len(encoded) <= 32<<20 || !json.Valid(encoded) {
				t.Fatal("full report above the former 32 MiB ceiling was lost", err)
			}
			compact, err := reporter.Encode(report, "pr-markdown", security.Redactor{})
			if err != nil || len(compact) > reporter.MaxPRMarkdownBytes {
				t.Fatal("large comparison broke the PR summary bound", err)
			}
		})
	}
}
