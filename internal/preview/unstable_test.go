package preview

import (
	"context"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

// unstableBackend renders a Secret whose password differed between two
// renders of identical inputs, as local external Helm marks it,
// and optionally a ConfigMap that changes between base and head.
type unstableBackend struct {
	limitedBackend
	change bool
}

func (b *unstableBackend) Render(_ context.Context, _ domain.Application, sha string) ([]string, error) {
	docs := []string{`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"password":"` + domain.RenderUnstable + `"}}`}
	mode := "same"
	if b.change {
		mode = sha
	}
	return append(docs, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"},"data":{"mode":"`+mode+`"}}`), nil
}

// A field that differed between two renders leaves its resource unknown and
// does not fail the Application; the other resources are still compared.
func TestUnstableFieldsDoNotFailTheApplication(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	for _, tc := range []struct {
		change                bool
		appState, configState string
	}{
		{true, "changed", "changed"},
		{false, "unknown", "unchanged"},
	} {
		report := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &unstableBackend{change: tc.change}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
		app := report.Applications[0]
		states := map[string]domain.ResourceDiff{}
		for _, res := range app.Resources {
			states[res.ID.Kind] = res
		}
		secret, config := states["Secret"], states["ConfigMap"]
		if report.Status != "success" || report.ChangesFound != tc.change || string(app.State) != tc.appState || app.Execution != "complete" || len(report.Issues) > 0 {
			t.Fatalf("change=%t: status %s, changes %t, app %s/%s, issues %v; want success, %s/complete", tc.change, report.Status, report.ChangesFound, app.State, app.Execution, report.Issues, tc.appState)
		}
		if secret.State != "unknown" || secret.Comparison != "incomplete" || !slices.Equal(secret.UnknownPaths, []string{"/data/password"}) || string(config.State) != tc.configState || config.Comparison != "complete" {
			t.Fatalf("change=%t: resources %+v", tc.change, app.Resources)
		}
	}
}

// A child rendered from a desired spec with a placeholder keeps its observed
// changes, is not proven unchanged, and names the placeholder in every
// report format.
func TestPlaceholderChildIsNotProvenUnchanged(t *testing.T) {
	worker := graphApp("worker")
	worker.Spec["source"].(map[string]any)["helm"] = map[string]any{"parameters": []any{map[string]any{"name": "token", "value": domain.RenderUnstable}}}
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			return []string{appManifest(worker)}, nil
		}
		mode := "old"
		if revision == graphHead {
			mode = "new"
		}
		return []string{configMap("same"), `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"mode"},"data":{"mode":"` + mode + `"}}`}, nil
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	child, root := findApp(t, report, "worker"), findApp(t, report, "root")
	if p := child.PlaceholderInput; p == nil || p.Application != worker.ID || !slices.Equal(p.Paths, []string{"/spec/source/helm/parameters/0/value"}) || child.Execution != "complete" || len(child.Issues) > 0 {
		t.Fatalf("child: %+v", child)
	}
	states := map[string]string{}
	for _, res := range child.Resources {
		states[res.ID.Name] = string(res.State) + "/" + string(res.Comparison)
	}
	if states["settings"] != "unknown/incomplete" || states["mode"] != "changed/incomplete" {
		t.Fatalf("child resources: %v", states)
	}
	if root.PlaceholderInput != nil || len(root.Resources) != 1 || !slices.Equal(root.Resources[0].UnknownPaths, []string{"/spec/source/helm/parameters/0/value"}) {
		t.Fatalf("root: %+v", root)
	}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := reporter.Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		want := "Rendered from placeholder values"
		if format == "json" {
			want = `"placeholderInput"`
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s does not name the placeholder:\n%s", format, data)
		}
	}
}
