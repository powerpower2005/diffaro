package preview

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// faultyBackend returns err when it renders Application name.
type faultyBackend struct {
	limitedBackend
	name string
	err  error
}

func (b *faultyBackend) CheckProject(context.Context, domain.Application) error { return nil }

func (b *faultyBackend) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	if app.ID.Name == b.name {
		return nil, b.err
	}
	return b.limitedBackend.Render(ctx, app, revision)
}

func runWithFault(t *testing.T, err error) domain.ApplicationResult {
	t.Helper()
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "5s", MaxConcurrency: 4, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	for _, name := range []string{"a", "b"} {
		c.Applications = append(c.Applications, domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: name})
	}
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &faultyBackend{name: "b", err: err}}}.Run(context.Background(), domain.RevisionPair{Base: graphBase, Head: graphHead})
	for _, app := range report.Applications {
		if app.ID.Name == "a" && app.Execution != domain.ExecutionComplete {
			t.Fatalf("independent Application lost its result: %+v", app)
		}
		if app.ID.Name == "b" {
			return app
		}
	}
	t.Fatal("faulty Application missing")
	return domain.ApplicationResult{}
}

// An error no adapter classified reaches the report as a Diffaro defect:
// internal_error with reason code unclassified, the bug report action and
// its cause chain, never its text.
func TestUnclassifiedRenderErrorIsReportedAsADefect(t *testing.T) {
	app := runWithFault(t, errors.New("fictional-canary-render"))
	if app.Execution != domain.ExecutionFailed || len(app.Issues) == 0 {
		t.Fatalf("app = %+v", app)
	}
	for _, issue := range app.Issues {
		if issue.Code != domain.CodeInternalError || issue.ReasonCode != domain.ReasonUnclassified || issue.Action != domain.DefectAction {
			t.Fatalf("issue = %+v", issue)
		}
		if issue.Diagnostic == nil || !slices.Equal(issue.Diagnostic.Chain, []string{"*errors.errorString"}) {
			t.Fatalf("diagnostic = %+v", issue.Diagnostic)
		}
	}
	encoded, _ := json.Marshal(app)
	if strings.Contains(string(encoded), "fictional-canary-render") {
		t.Fatalf("error text reached the report: %s", encoded)
	}
}

// A classified failure keeps the line that created it.
func TestClassifiedRenderErrorKeepsItsSite(t *testing.T) {
	app := runWithFault(t, domain.Fail(domain.CodeRenderFailed, "render", "fictional render failure", "check the chart", nil))
	if len(app.Issues) == 0 {
		t.Fatalf("app = %+v", app)
	}
	for _, issue := range app.Issues {
		if issue.Code != domain.CodeRenderFailed || issue.Diagnostic == nil || !strings.HasPrefix(issue.Diagnostic.Site, "preview/unclassified_test.go:") {
			t.Fatalf("issue = %+v diagnostic = %+v", issue, issue.Diagnostic)
		}
	}
}
