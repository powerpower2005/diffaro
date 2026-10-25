package preview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type ignoreRuleBackend struct{ respect bool }

func (b ignoreRuleBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	spec := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "helm": map[string]any{"releaseName": "api"}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"},
		"ignoreDifferences": []any{map[string]any{"kind": "Secret", "jsonPointers": []any{"/data"}}}}
	if b.respect {
		spec["syncPolicy"] = map[string]any{"syncOptions": []any{"RespectIgnoreDifferences=true"}}
	}
	return domain.Application{ID: id, Spec: spec}, nil
}

func (b ignoreRuleBackend) Render(_ context.Context, _ domain.Application, revision string) ([]string, error) {
	return []string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo","namespace":"demo"},"data":{"revision":"` + revision + `"}}`}, nil
}

// Without RespectIgnoreDifferences, ignore rules change only live drift status,
// so targets are compared and the unapplied rules are stated; with it, the
// Application stays explicitly unsupported.
func TestIgnoreRulesAreComparedOnlyWhenTheyDoNotChangeAppliedTargets(t *testing.T) {
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10},
		Applications: []domain.ApplicationID{{Instance: "test", Namespace: "argocd", Name: "api"}}}
	pair := domain.RevisionPair{Base: "base", Head: "head"}

	r := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": ignoreRuleBackend{}}}.Run(context.Background(), pair)
	app := r.Applications[0]
	if r.Status != "success" || app.Execution != "complete" || app.State != "changed" || app.Inputs.IgnoreDifferences != 1 {
		t.Fatalf("ignore rules without RespectIgnoreDifferences must not block comparison: status=%s execution=%s state=%s inputs=%d", r.Status, app.Execution, app.State, app.Inputs.IgnoreDifferences)
	}
	stated := false
	for _, limitation := range r.Limitations {
		stated = stated || strings.Contains(limitation, "were not applied")
	}
	if !stated {
		t.Fatal("unapplied ignore rules must be stated in the report")
	}

	r = Runner{Config: c, Renderers: map[string]domain.Renderer{"test": ignoreRuleBackend{respect: true}}}.Run(context.Background(), pair)
	app = r.Applications[0]
	if app.Execution != "failed" || len(app.Issues) == 0 || app.Issues[0].Code != "unsupported_ignore_rules" {
		t.Fatalf("RespectIgnoreDifferences must stay unsupported: execution=%s issues=%v", app.Execution, app.Issues)
	}
}

func TestSupportedIgnoreRules(t *testing.T) {
	base, _ := ignoreRuleBackend{}.Application(context.Background(), domain.ApplicationID{})
	if err := Supported(base); err != nil {
		t.Fatalf("ignore rules without RespectIgnoreDifferences rejected: %v", err)
	}
	respected, _ := ignoreRuleBackend{respect: true}.Application(context.Background(), domain.ApplicationID{})
	var failure *domain.Failure
	if err := Supported(respected); !errors.As(err, &failure) || failure.Issue.Code != "unsupported_ignore_rules" {
		t.Fatalf("RespectIgnoreDifferences accepted: %v", err)
	}
}
