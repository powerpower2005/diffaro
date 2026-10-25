package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

const g68MaskedSecret = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"password":"++++++++"}}`

func g68AssertMaskedOnly(t *testing.T, report domain.Report, app domain.ApplicationResult) {
	t.Helper()
	if app.State != "unknown" {
		t.Fatalf("an Application whose only target is a masked Secret must be unknown, got state %q", app.State)
	}
	if app.Execution == "failed" || report.Status == "partial_failure" || report.Status == "failure" || report.ExitCode() == 3 {
		t.Fatalf("a masked Secret must not fail the Application or the run: execution=%q status=%q exit=%d", app.Execution, report.Status, report.ExitCode())
	}
	if len(app.Resources) != 1 || app.Resources[0].State != "unknown" {
		t.Fatalf("the masked Secret must stay one unknown resource: %+v", app.Resources)
	}
}

// An explicit Application whose only target is a Secret masked by
// Argo CD is unknown, not unchanged and not failed.
func TestG68ExplicitRunMaskedSecretOnlyIsUnknown(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &secretBackend{}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if len(report.Applications) != 1 {
		t.Fatalf("applications: %+v", report.Applications)
	}
	g68AssertMaskedOnly(t, report, report.Applications[0])
}

// The recursive run applies the same rule to a graph node.
func TestG68RecursiveRunMaskedSecretOnlyIsUnknown(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(context.Context, domain.Application, string) ([]string, error) {
		return []string{g68MaskedSecret}, nil
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	g68AssertMaskedOnly(t, report, findApp(t, report, "root"))
}
