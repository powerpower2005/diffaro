package preview

import (
	"context"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOmittedHelmConfigurationExplainsArgoSerialization(t *testing.T) {
	app := domain.Application{Spec: map[string]any{"source": map[string]any{"repoURL": "https://github.com/example/charts.git", "path": "workload"}}}
	var failure *domain.Failure
	if err := Supported(app); !errors.As(err, &failure) || failure.Issue.Code != "unsupported_source" || !strings.Contains(failure.Issue.Action, "empty helm: {}") {
		t.Fatal("an omitted Helm config must fail explicitly with an actionable serialization explanation")
	}
}

type limitedBackend struct {
	active, peak atomic.Int32
	timeout      bool
}

func (b *limitedBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	return domain.Application{ID: id, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"}}}, nil
}
func (b *limitedBackend) Render(ctx context.Context, _ domain.Application, _ string) ([]string, error) {
	if b.timeout {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		peak := b.peak.Load()
		if active <= peak || b.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	return []string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo"}}`}, nil
}

func TestBoundedConcurrency(t *testing.T) {
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 2, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	for i := 0; i < 12; i++ {
		c.Applications = append(c.Applications, domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: fmt.Sprintf("app-%02d", i)})
	}
	b := &limitedBackend{}
	r := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": b}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if b.peak.Load() > 2 || len(r.Applications) != 12 || r.Status != "success" {
		t.Fatal("concurrency bound or independent completion failed")
	}
}

func TestTimeoutIsActionable(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1ms", MaxConcurrency: 1}}
	r := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &limitedBackend{timeout: true}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if r.Status != "failed" || r.Applications[0].Issues[0].Code != "timeout" || r.Applications[0].Issues[0].Revision != "base" {
		t.Fatal("timeout lost context")
	}
	issue := ContextualIssue(fmt.Errorf("wrapped: %w", context.Canceled), id, "repo", "sha")
	if issue.Code != "cancelled" {
		t.Fatal("cancel cause not recognized")
	}
	wrapped := domain.Fail("x", "stage", "safe", "retry", context.Canceled)
	if !errors.Is(wrapped, context.Canceled) {
		t.Fatal("typed error cause lost")
	}
}

func TestRetainedReportBudget(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	r := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &limitedBackend{}}, maxResultBytes: 1}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if r.Status != "failed" || len(r.Applications[0].Resources) != 0 || r.Applications[0].Issues[0].Code != "report_limit_exceeded" {
		t.Fatal("oversized results were retained or reported as no changes")
	}
}

type checkOrderBackend struct {
	app            domain.Application
	projectChecks  atomic.Int32
	singleRendered atomic.Int32
}

func (b *checkOrderBackend) Application(context.Context, domain.ApplicationID) (domain.Application, error) {
	return b.app, nil
}
func (b *checkOrderBackend) Render(context.Context, domain.Application, string) ([]string, error) {
	b.singleRendered.Add(1)
	return nil, nil
}
func (b *checkOrderBackend) CheckProject(context.Context, domain.Application) error {
	b.projectChecks.Add(1)
	return domain.Fail(domain.CodeAccessDenied, "permission", "project denied", "allow project read", nil)
}

// TestExplicitAndGraphModesShareCheckOrder pins one check order for an
// Application that fails several checks: support before destination, and
// source planning and the multi-source capability before the project check.
func TestExplicitAndGraphModesShareCheckOrder(t *testing.T) {
	id := domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "app"}
	for _, tc := range []struct {
		name, code, stage string
		spec              map[string]any
	}{
		{"unsupported source without destination", domain.CodeUnsupportedSource, "selection", map[string]any{"project": "demo", "source": map[string]any{"repoURL": graphRepo, "path": "chart"}}},
		{"multi-source capability before project", domain.CodeUnsupportedSource, "render", map[string]any{"project": "demo", "destination": map[string]any{"name": "cluster", "namespace": "demo"}, "sources": []any{
			map[string]any{"repoURL": graphRepo, "path": "chart", "targetRevision": "main", "helm": map[string]any{}},
			map[string]any{"repoURL": graphRepo, "targetRevision": "main", "ref": "values"},
		}}},
	} {
		for _, recursive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recursive=%v", tc.name, recursive), func(t *testing.T) {
				backend := &checkOrderBackend{app: domain.Application{ID: id, Spec: tc.spec}}
				c := profile.Config{Git: profile.Git{RepositoryURL: graphRepo}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxDepth: 8, MaxApplications: 10, MaxDocuments: 10, MaxResponseBytes: 1 << 20}}
				runner := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": backend}}
				var report domain.Report
				if recursive {
					report = runner.RunRecursive(context.Background(), graphPair())
				} else {
					report = runner.Run(context.Background(), graphPair())
				}
				app := report.Applications[0]
				if app.Execution != domain.ExecutionFailed || len(app.Issues) == 0 || app.Issues[0].Code != tc.code || app.Issues[0].Stage != tc.stage {
					t.Fatalf("an Application failing several checks must report the first check of the shared order: %+v", app.Issues)
				}
				if backend.projectChecks.Load() != 0 || backend.singleRendered.Load() != 0 {
					t.Fatal("a later check or render ran after an earlier check failed")
				}
			})
		}
	}
}

// A cancelled or timed-out step gets the action of its real cause, also when
// the error it interrupted, such as a request, carried its own action.
func TestCancelledAndTimedOutStepsGetTheirOwnAction(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	interrupted := domain.FailReason(domain.CodeConnectionFailed, "fictional_reason", "connection", "request failed", "check connectivity, TLS and timeout settings", context.Canceled)
	for _, tc := range []struct {
		err          error
		code, action string
	}{
		{context.Canceled, domain.CodeCancelled, "rerun the preview"},
		{interrupted, domain.CodeCancelled, "rerun the preview"},
		{fmt.Errorf("render: %w", context.DeadlineExceeded), domain.CodeTimeout, "raise render.timeout"},
	} {
		issue := ContextualIssue(tc.err, id, "repo", "sha")
		if issue.Code != tc.code || !strings.Contains(issue.Action, tc.action) || issue.ReasonCode != "" || strings.Contains(issue.Action, "connectivity") || strings.Contains(issue.Action, "safe diagnostics") {
			t.Fatalf("%v: %+v", tc.err, issue)
		}
	}
	if issue := ContextualIssue(interrupted, id, "repo", "sha"); issue.Stage != "connection" {
		t.Fatalf("the interrupted step's stage is kept: %+v", issue)
	}
}
