package preview

import (
	"context"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

type repeatedBackend struct{}

func (repeatedBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	return domain.Application{ID: id, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"}}}, nil
}

// Two templates render the same identity; the second differs between revisions.
func (repeatedBackend) Render(_ context.Context, _ domain.Application, sha string) ([]string, error) {
	return []string{"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: shared}\ndata: {from: first}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: shared}\ndata: {from: second-" + sha + "}\n"}, nil
}

func TestSingleSourceRepeatedResourcesUseLastOccurrence(t *testing.T) {
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxApplications: 10, MaxDepth: 4, MaxResponseBytes: 1 << 20, MaxDocuments: 10}, Applications: []domain.ApplicationID{{Instance: "test", Namespace: "argocd", Name: "shared"}}}
	pair := domain.RevisionPair{Base: "base", Head: "head"}
	runner := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": repeatedBackend{}}}
	for name, report := range map[string]domain.Report{"run": runner.Run(context.Background(), pair), "recursive": runner.RunRecursive(context.Background(), pair)} {
		t.Run(name, func(t *testing.T) {
			if report.Status != "success" || len(report.Applications) != 1 || len(report.Applications[0].Resources) != 1 {
				t.Fatalf("repeated single-source target failed: %+v", report)
			}
			app := report.Applications[0]
			changes := app.Resources[0].Changes
			if len(changes) != 1 || changes[0].Before != "second-base" || changes[0].After != "second-head" {
				t.Fatalf("last occurrence not compared: %+v", changes)
			}
			base, head := app.BaseRepeatedResources, app.HeadRepeatedResources
			if app.BaseTarget != nil {
				base, head = app.BaseTarget.RepeatedResources, app.HeadTarget.RepeatedResources
			}
			if len(base) != 1 || len(head) != 1 || base[0].Name != "shared" {
				t.Fatalf("repeated identities not recorded: %v %v", base, head)
			}
			terminal, err := reporter.Encode(report, "terminal", security.Redactor{})
			if err != nil || !strings.Contains(string(terminal), "Repeated in head target: 1 resources; the last rendered occurrence is compared") {
				t.Fatalf("terminal hides repeated resources: %v\n%s", err, terminal)
			}
			markdown, err := reporter.Encode(report, "markdown", security.Redactor{})
			if err != nil || !strings.Contains(string(markdown), "**Repeated resources: 1.**") || strings.Contains(string(markdown), "sources for") {
				t.Fatalf("markdown repeated context wrong: %v\n%s", err, markdown)
			}
		})
	}
}
