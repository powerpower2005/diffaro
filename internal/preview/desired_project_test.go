package preview

import (
	"context"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
)

type projectCheckingGraphBackend struct {
	graphBackend
	checked, desired atomic.Int32
}

func (b *projectCheckingGraphBackend) CheckProject(_ context.Context, app domain.Application) error {
	if app.Project() == "restricted" {
		return domain.Fail(domain.CodeAccessDenied, "permission", "project denied", "allow project read", nil)
	}
	b.checked.Add(1)
	return nil
}
func (b *projectCheckingGraphBackend) RenderSources(context.Context, domain.Application, []domain.SourceRevision) ([]string, error) {
	return nil, domain.Fail(domain.CodeUnsupportedSource, "render", "stored multi-source render not expected", "", nil)
}
func (b *projectCheckingGraphBackend) RenderDesiredSources(context.Context, domain.Application, []domain.SourceRevision) ([]string, error) {
	if b.checked.Load() == 0 {
		return nil, domain.Fail(domain.CodeAccessDenied, "permission", "rendered before the AppProject check", "", nil)
	}
	b.desired.Add(1)
	return []string{configMap("child")}, nil
}

// Preview checks the AppProject of a desired multi-source child before
// rendering it, so backends need not read it again: a denied project fails
// the child without any render.
func TestDesiredMultiSourceChildIsProjectCheckedBeforeRendering(t *testing.T) {
	for _, project := range []string{"demo", "restricted"} {
		t.Run(project, func(t *testing.T) {
			child := graphApp("child")
			delete(child.Spec, "source")
			child.Spec["project"] = project
			child.Spec["sources"] = []any{
				map[string]any{"repoURL": graphRepo, "path": "charts/child", "targetRevision": "main", "helm": map[string]any{"valueFiles": []any{"$values/prod.yaml"}}},
				map[string]any{"repoURL": graphRepo, "targetRevision": "main", "ref": "values"},
			}
			backend := &projectCheckingGraphBackend{graphBackend: graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(context.Context, domain.Application, string) ([]string, error) {
				return []string{appManifest(child)}, nil
			}}}
			runner := graphRunner(backend.graphBackend, "root")
			runner.Renderers["one"] = backend
			result := findApp(t, runner.RunRecursive(context.Background(), graphPair()), "child")
			if project == "restricted" {
				if result.Execution != domain.ExecutionDenied || len(result.Issues) == 0 || result.Issues[0].Code != domain.CodeAccessDenied || backend.desired.Load() != 0 {
					t.Fatalf("denied AppProject did not fail the child before rendering: %+v desired=%d", result.Issues, backend.desired.Load())
				}
				return
			}
			if result.Execution != domain.ExecutionComplete || backend.desired.Load() != 2 {
				t.Fatalf("checked child not rendered on both sides: %s %+v desired=%d", result.Execution, result.Issues, backend.desired.Load())
			}
		})
	}
}
