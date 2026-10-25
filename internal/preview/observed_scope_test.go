package preview

import (
	"context"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type observedScopeBackend struct{ scopes map[string]bool }

func (b observedScopeBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	return domain.Application{ID: id, ObservedScopes: b.scopes, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"}}}, nil
}
func (b observedScopeBackend) Render(_ context.Context, _ domain.Application, sha string) ([]string, error) {
	return []string{"apiVersion: iam.services.k8s.aws/v1alpha1\nkind: Role\nmetadata: {name: ack-role}\nspec: {description: " + sha + "}\n"}, nil
}

func TestObservedScopeCompletesComparisonAndIsDisclosed(t *testing.T) {
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxApplications: 10, MaxDepth: 4, MaxResponseBytes: 1 << 20, MaxDocuments: 10}, Applications: []domain.ApplicationID{{Instance: "test", Namespace: "argocd", Name: "iam"}}}
	pair := domain.RevisionPair{Base: "base", Head: "head"}
	for name, run := range map[string]func(Runner) domain.Report{
		"run":       func(r Runner) domain.Report { return r.Run(context.Background(), pair) },
		"recursive": func(r Runner) domain.Report { return r.RunRecursive(context.Background(), pair) },
	} {
		t.Run(name, func(t *testing.T) {
			report := run(Runner{Config: c, Renderers: map[string]domain.Renderer{"test": observedScopeBackend{scopes: map[string]bool{"iam.services.k8s.aws/Role": false}}}})
			if report.Status != "success" || len(report.Applications) != 1 || len(report.Applications[0].Resources) != 1 {
				t.Fatalf("comparison did not complete: %+v", report)
			}
			resource := report.Applications[0].Resources[0]
			if resource.ID.Namespace != "demo" || resource.State != "changed" || resource.ScopeSource != "argocd-observed" || !strings.Contains(strings.Join(report.Limitations, "\n"), "scopeSource: argocd-observed") {
				t.Fatalf("observed scope not disclosed: %+v %v", resource, report.Limitations)
			}

			report = run(Runner{Config: c, Renderers: map[string]domain.Renderer{"test": observedScopeBackend{}}})
			if report.Status == "success" || strings.Contains(strings.Join(report.Limitations, "\n"), "argocd-observed") {
				t.Fatalf("unknown scope was not a failure: %+v", report)
			}
			found := false
			for _, issue := range append(report.Issues, report.Applications[0].Issues...) {
				found = found || issue.Code == "scope_unavailable"
			}
			if !found {
				t.Fatalf("scope_unavailable missing: %+v", report)
			}
		})
	}
}
