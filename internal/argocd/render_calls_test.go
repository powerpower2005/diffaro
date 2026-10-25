package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
	"diffaro/internal/revision"
)

// renderCallServer serves one Application and counts read-only calls. When
// driftAfterRender is set, the Application spec read after the manifest
// request differs from the one read before it.
func renderCallServer(t *testing.T, spec map[string]any, driftAfterRender bool) (*httptest.Server, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var apps, projects, renders atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("mutation attempted")
		}
		switch r.URL.Path {
		case "/api/v1/applications/child":
			apps.Add(1)
			current := spec
			if driftAfterRender && renders.Load() > 0 {
				current = domain.CloneSpec(spec)
				current["project"] = "moved"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "child", "namespace": "argocd"}, "spec": current})
		case "/api/v1/applications/child/manifests":
			renders.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`}})
		case "/api/v1/projects/demo":
			projects.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	return server, &apps, &projects, &renders
}

func TestDesiredRenderReusesStoredApplicationAsDriftSnapshot(t *testing.T) {
	single := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://example.com/team/chart", "path": "chart", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster"}}
	multi := map[string]any{"project": "demo", "destination": map[string]any{"name": "test-cluster"}, "sources": []any{
		map[string]any{"repoURL": "https://example.com/team/chart", "path": "chart", "targetRevision": strings.Repeat("a", 40), "helm": map[string]any{}},
		map[string]any{"repoURL": "https://example.com/team/values", "ref": "values", "targetRevision": "main"},
	}}
	cases := []struct {
		name   string
		spec   map[string]any
		render func(context.Context, *Client, domain.Application) ([]string, error)
		// apps counts Application GETs: one stored read and one post-render read.
		apps, projects int32
	}{
		{"single-source desired", single, func(ctx context.Context, c *Client, app domain.Application) ([]string, error) {
			return c.RenderDesired(ctx, app, strings.Repeat("b", 40))
		}, 2, 1},
		// Preview checks a multi-source target's AppProject before this call
		// (TestDesiredMultiSourceChildIsProjectCheckedBeforeRendering).
		{"multi-source desired", multi, func(ctx context.Context, c *Client, app domain.Application) ([]string, error) {
			plan, err := revision.PlanSources(ctx, app, "https://example.com/team/values", strings.Repeat("b", 40), nil)
			if err != nil {
				return nil, err
			}
			return c.RenderDesiredSources(ctx, app, plan)
		}, 2, 0},
		{"single-source stored", single, func(ctx context.Context, c *Client, app domain.Application) ([]string, error) {
			return c.Render(ctx, app, strings.Repeat("b", 40))
		}, 2, 0},
	}
	for _, tc := range cases {
		for _, drift := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/stable", true: "/drift"}[drift], func(t *testing.T) {
				server, apps, projects, renders := renderCallServer(t, tc.spec, drift)
				defer server.Close()
				c := &Client{Instance: "test", HTTP: httpx.New(server.URL, "", time.Second, 1<<20)}
				app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "child"}, Spec: domain.CloneSpec(tc.spec)}
				out, err := tc.render(context.Background(), c, app)
				if apps.Load() != tc.apps || projects.Load() != tc.projects || renders.Load() != 1 {
					t.Fatalf("calls: applications=%d projects=%d renders=%d", apps.Load(), projects.Load(), renders.Load())
				}
				var failure *domain.Failure
				if drift {
					if !errors.As(err, &failure) || failure.Issue.Code != "render_context_changed" || out != nil {
						t.Fatalf("drift after render was not detected: %v", err)
					}
					return
				}
				if err != nil || len(out) != 1 {
					t.Fatalf("render failed: %v %v", out, err)
				}
			})
		}
	}
}
