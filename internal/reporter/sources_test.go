package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestMultiSourceContextAndRepeatedResourcesInAllReports(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		report := domain.NewReport()
		id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "shop"}
		sources := []domain.SourceSummary{{Position: 1, Repository: "https://example.com/chart", Revision: strings.Repeat("a", 40), Rule: "source-commit", Inputs: domain.InputSummary{Path: "chart", ReleaseName: "shop", ValueFiles: []string{"$values/prod.yaml"}}}, {Position: 2, Repository: "https://example.com/values", Ref: "values", Revision: strings.Repeat("b", 40), Rule: "selected-repository-pair"}}
		repeated := []domain.ResourceID{{Kind: "ConfigMap", Namespace: "demo", Name: "shared"}}
		app := domain.ApplicationResult{ID: id, Execution: "complete", State: "unchanged", BaseSources: sources, HeadSources: sources, BaseRepeatedResources: repeated, HeadRepeatedResources: repeated}
		if recursive {
			app.BaseSources, app.HeadSources, app.BaseRepeatedResources, app.HeadRepeatedResources = nil, nil, nil, nil
			app.BaseTarget = &domain.TargetSummary{Sources: sources, RepeatedResources: repeated}
			app.HeadTarget = &domain.TargetSummary{Sources: sources, RepeatedResources: repeated}
		}
		report.Applications = []domain.ApplicationResult{app}
		for _, format := range []string{"json", "markdown", "pr-markdown"} {
			data, err := Encode(report, format, security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"https://example.com/chart", "https://example.com/values", "$values/prod.yaml", "shared", strings.Repeat("b", 40)} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("%s missing source evidence %q", format, want)
				}
			}
			if format != "json" && !strings.Contains(string(data), "last rendered occurrence wins") {
				t.Fatal("repeated resource precedence is silent")
			}
		}
	}
}
