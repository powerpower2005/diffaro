package localhelm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// A read denied after rendering names the Application's known AppProject in
// the required access. The post-render Application read reports exactly what
// the Argo CD API backend reports for the same denied read, not "AppProject
// is unresolved".
func TestDriftReadDenialNamesKnownProject(t *testing.T) {
	req := fixtureRequest()
	app := req.Application
	// api serves Argo CD with the second Application or AppProject read denied.
	api := func(t *testing.T, denied string) *argocd.Client {
		appReads, projectReads := 0, 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/manifests"):
				json.NewEncoder(w).Encode(map[string]any{"manifests": []string{}, "sourceType": "Helm"})
			case strings.HasPrefix(r.URL.Path, "/api/v1/applications/"):
				if appReads++; denied == "application" && appReads > 1 {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": app.ID.Name, "namespace": app.ID.Namespace}, "spec": app.Spec})
			case r.URL.Path == "/api/v1/projects/demo":
				if projectReads++; denied == "project" && projectReads > 1 {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": []any{"*"}, "destinations": []any{map[string]any{"name": "*", "namespace": "*"}}}})
			case r.URL.Path == "/api/v1/settings":
				json.NewEncoder(w).Encode(req.Settings)
			case strings.HasPrefix(r.URL.Path, "/api/v1/clusters/"):
				json.NewEncoder(w).Encode(map[string]any{"server": "https://cluster.example.invalid", "name": "service-east", "info": map[string]any{"serverVersion": "1.31.0", "apiVersions": []string{"v1", "apps/v1/Deployment"}, "connectionState": map[string]any{"status": "Successful"}}})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)
		return &argocd.Client{Instance: app.ID.Instance, HTTP: httpx.New(server.URL, "fictional-token", time.Second, 1<<20)}
	}
	render := func(t *testing.T, denied string) []domain.Issue {
		client := &Client{API: api(t, denied), Git: fixtureGit{files: map[string]map[string][]byte{"example/charts@" + testSHA: req.Repositories[testRepo+"@"+testSHA]}}, RepositoryURL: testRepo, Limit: 1 << 20, Execute: func(_ context.Context, r Request) ([]string, error) { return Render(r) }}
		_, err := client.Render(context.Background(), app, testSHA)
		issues := domain.RenderIssues(err)
		if len(issues) != 1 || issues[0].Code != domain.CodeAccessDenied || issues[0].RequiredAccess == nil {
			t.Fatalf("denied drift read was not an access failure: %v", err)
		}
		return issues
	}

	t.Run("application", func(t *testing.T) {
		got := render(t, "application")
		_, err := api(t, "application").Render(context.Background(), app, testSHA)
		want := domain.RenderIssues(err)
		if len(want) != 1 || !reflect.DeepEqual(got[0], want[0]) {
			t.Fatalf("local Helm drift read diagnostic differs from the Argo CD backend:\n got %+v %+v\nwant %+v", got, got[0].RequiredAccess, want)
		}
		if required := got[0].RequiredAccess; !strings.Contains(required.Target, "project=demo") || strings.Contains(required.Scope, "unresolved") {
			t.Fatalf("known AppProject reported as unresolved: %+v", required)
		}
	})
	t.Run("project", func(t *testing.T) {
		if required := render(t, "project")[0].RequiredAccess; required.Resource != "projects" || required.Target != "demo" {
			t.Fatalf("AppProject drift read lost the known project: %+v", required)
		}
	})
}
