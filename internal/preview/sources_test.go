package preview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type multiPreviewBackend struct {
	app   domain.Application
	calls int
	t     *testing.T
}

func TestMultiSourceDiscoveryIgnoresInactiveSingleSource(t *testing.T) {
	app := domain.Application{Spec: map[string]any{"source": map[string]any{"repoURL": "https://example.com/ignored"}, "sources": []any{map[string]any{"repoURL": "https://example.com/chart"}, map[string]any{"repoURL": "https://example.com/values", "ref": "values"}}}}
	if referencesRepository(app, "https://example.com/ignored") || !referencesRepository(app, "https://example.com/values") {
		t.Fatal("discovery did not use effective sources")
	}
}

func TestDiscoveryMatchesOnlyExplicitRepositoryAliases(t *testing.T) {
	app := domain.Application{Spec: map[string]any{"source": map[string]any{"repoURL": "https://mirror.example.com/example/charts"}}}
	if referencesRepository(app, "https://github.com/example/charts") {
		t.Fatal("mirror host matched without an alias")
	}
	if !referencesRepository(app, "https://github.com/example/charts", "https://mirror.example.com/example/charts.git") {
		t.Fatal("explicit alias did not select the mirror source")
	}
	if referencesRepository(app, "https://github.com/example/charts", "https://mirror.example.com/example/other") {
		t.Fatal("alias for another repository selected the source")
	}
}

type resolvingPreviewBackend struct {
	*multiPreviewBackend
	resolutions int
}

func (b *resolvingPreviewBackend) ResolveSourceRevision(context.Context, domain.Application, string, string) (string, error) {
	b.resolutions++
	// A second resolution would simulate a branch moving while the PR is read.
	if b.resolutions%2 == 0 {
		return strings.Repeat("d", 40), nil
	}
	return strings.Repeat("c", 40), nil
}

func (b *multiPreviewBackend) Application(context.Context, domain.ApplicationID) (domain.Application, error) {
	return b.app, nil
}
func (b *multiPreviewBackend) Render(context.Context, domain.Application, string) ([]string, error) {
	b.t.Fatal("single-source renderer used")
	return nil, nil
}
func (b *multiPreviewBackend) RenderSources(_ context.Context, _ domain.Application, plan []domain.SourceRevision) ([]string, error) {
	b.calls++
	if len(plan) != 2 || plan[0].Revision != strings.Repeat("c", 40) || plan[1].Position != 2 {
		b.t.Fatal("source pinning changed")
	}
	return []string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shop"},"data":{"shadowed":"earlier-source"}}`, fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shop"},"data":{"revision":%q}}`, plan[1].Revision)}, nil
}
func (b *multiPreviewBackend) RenderDesiredSources(ctx context.Context, a domain.Application, plan []domain.SourceRevision) ([]string, error) {
	return b.RenderSources(ctx, a, plan)
}

func TestMultiSourcePreviewTargetsAndProvenance(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "shop"}
			app := domain.Application{ID: id, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "cluster", "namespace": "demo"}, "sources": []any{
				map[string]any{"repoURL": "https://github.com/example/chart", "path": "chart", "targetRevision": strings.Repeat("c", 40), "helm": map[string]any{"releaseName": "shop", "valueFiles": []any{"$values/prod.yaml"}}},
				map[string]any{"repoURL": "https://github.com/example/values", "targetRevision": "main", "ref": "values"},
			}}}
			backend := &multiPreviewBackend{app: app, t: t}
			config := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/values"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxDepth: 8, MaxApplications: 10, MaxDocuments: 10, MaxResponseBytes: 1 << 20}}
			runner := Runner{Config: config, Renderers: map[string]domain.Renderer{"test": backend}}
			pair := domain.RevisionPair{Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
			var report domain.Report
			if recursive {
				report = runner.RunRecursive(context.Background(), pair)
			} else {
				report = runner.Run(context.Background(), pair)
			}
			if report.Status != "success" || !report.ChangesFound || backend.calls != 2 || len(report.Applications) != 1 {
				t.Fatalf("multi preview failed: %+v", report)
			}
			result := report.Applications[0]
			repeatedBase, repeatedHead := result.BaseRepeatedResources, result.HeadRepeatedResources
			if recursive {
				repeatedBase, repeatedHead = result.BaseTarget.RepeatedResources, result.HeadTarget.RepeatedResources
			}
			if len(repeatedBase) != 1 || len(repeatedHead) != 1 || len(result.Resources) != 1 {
				t.Fatal("repeated-resource evidence missing from preview")
			}
			base, head := result.BaseSources, result.HeadSources
			if recursive {
				base, head = result.BaseTarget.Sources, result.HeadTarget.Sources
			}
			if len(base) != 2 || len(head) != 2 || base[1].Revision != pair.Base || head[1].Revision != pair.Head || head[0].Inputs.ReleaseName != "shop" || head[1].Ref != "values" {
				t.Fatal("source provenance missing")
			}
		})
	}
}

func TestMultiSourceMutableRevisionFrozenAcrossTargets(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "shop"}
			app := domain.Application{ID: id, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "cluster", "namespace": "demo"}, "sources": []any{
				map[string]any{"repoURL": "https://example.com/chart", "path": "chart", "targetRevision": "main", "helm": map[string]any{}},
				map[string]any{"repoURL": "https://example.com/values", "ref": "values", "targetRevision": "main"},
			}}}
			backend := &resolvingPreviewBackend{multiPreviewBackend: &multiPreviewBackend{app: app, t: t}}
			runner := Runner{Config: profile.Config{Git: profile.Git{RepositoryURL: "https://example.com/values"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxDepth: 8, MaxApplications: 10, MaxDocuments: 10, MaxResponseBytes: 1 << 20}}, Renderers: map[string]domain.Renderer{"test": backend}}
			pair := domain.RevisionPair{Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
			for run := 0; run < 2; run++ {
				backend.resolutions = 0
				var report domain.Report
				if recursive {
					report = runner.RunRecursive(context.Background(), pair)
				} else {
					report = runner.Run(context.Background(), pair)
				}
				if report.Status != "success" || backend.resolutions != 1 {
					t.Fatalf("revision not frozen independently per run: %d, %+v", backend.resolutions, report)
				}
				result := report.Applications[0]
				base, head := result.BaseSources, result.HeadSources
				if recursive {
					base, head = result.BaseTarget.Sources, result.HeadTarget.Sources
				}
				if base[0].Rule != "resolved-source-reference" || head[0].Revision != base[0].Revision {
					t.Fatal("frozen resolution provenance missing")
				}
			}
		})
	}
}
