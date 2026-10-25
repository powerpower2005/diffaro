package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const fakeDemoProject = `{"metadata":{"name":"demo"}}`

// argocdFake describes a fake Argo CD API backend behind readOnlyFake, with
// the instance's bearer token. Shared metadata endpoints are served unless before
// handles the request first; all other paths go to apps.
type argocdFake struct {
	token   string
	version string // /api/version Version; defaults to v3.5.3
	project string // /api/v1/projects/demo body; defaults to fakeDemoProject
	// before runs after the read-only/credential guard and before the shared
	// endpoints. Returning true means it fully handled the request.
	before func(http.ResponseWriter, *http.Request) bool
	apps   http.HandlerFunc
}

func newArgoCDFake(t *testing.T, f argocdFake) *httptest.Server {
	t.Helper()
	version, project := f.version, f.project
	if version == "" {
		version = "v3.5.3"
	}
	if project == "" {
		project = fakeDemoProject
	}
	return readOnlyFake(t, "Argo CD", f.token, func(w http.ResponseWriter, r *http.Request) {
		if f.before != nil && f.before(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprintf(w, `{"Version":%q}`, version)
		case "/api/v1/projects/demo":
			fmt.Fprint(w, project)
		default:
			f.apps(w, r)
		}
	})
}

// overlayArgoCD serves the given exact paths and delegates every other request
// to base, so a test can replace one endpoint of an existing fake. Mutating
// requests fail the test on every path.
func overlayArgoCD(t *testing.T, base *httptest.Server, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Argo CD fake: mutation attempted: %s %s", r.Method, r.URL.Path)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		if route, ok := routes[r.URL.Path]; ok {
			route(w, r)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// applicationPath splits /api/v1/applications/<name>[/manifests].
func applicationPath(path string) (name string, render bool) {
	name = strings.TrimPrefix(path, "/api/v1/applications/")
	render = strings.HasSuffix(name, "/manifests")
	return strings.TrimSuffix(name, "/manifests"), render
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func writeInventory(w http.ResponseWriter, items ...any) {
	writeJSON(w, map[string]any{"items": items})
}

func writeManifests(w http.ResponseWriter, manifests []string) {
	writeJSON(w, map[string]any{"manifests": manifests})
}

type backendOptions struct {
	denied      bool
	drift       bool
	secret      bool
	headFailure bool
	unaffected  bool
	unchanged   bool
	unsupported bool
	workload    bool
	remove      bool
}

func argoServer(t *testing.T, token string, opts backendOptions) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	revisions := []string{}
	renderCount := 0
	s := newArgoCDFake(t, argocdFake{token: token, project: `{"metadata":{"name":"demo"},"spec":{"sourceRepos":["https://github.com/example/charts.git"]}}`, before: func(w http.ResponseWriter, r *http.Request) bool {
		if opts.denied {
			http.Error(w, "fictional-sensitive-error", http.StatusForbidden)
		}
		return opts.denied
	}, apps: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/applications/api":
			if r.URL.Query().Get("appNamespace") != "argocd" {
				t.Error("namespace missing")
			}
			a := testApp()
			spec := a["spec"].(map[string]any)
			source := spec["source"].(map[string]any)
			if opts.drift && renderCount > 0 {
				source["helm"].(map[string]any)["releaseName"] = "different-release"
			}
			if opts.unaffected {
				source["repoURL"] = "https://github.com/example/other.git"
			}
			if opts.unsupported {
				source["plugin"] = map[string]any{"name": "unreviewed"}
			}
			writeJSON(w, a)
		case "/api/v1/applications/api/manifests":
			if r.URL.Query().Get("appNamespace") != "argocd" || r.URL.Query().Get("project") != "demo" {
				t.Error("render identity not preserved")
			}
			rev := r.URL.Query().Get("revision")
			revisions = append(revisions, rev)
			renderCount++
			if rev != baseSHA && rev != headSHA && rev != ancestorSHA {
				t.Error("render did not receive immutable SHA")
			}
			if opts.headFailure && rev == headSHA {
				http.Error(w, "fictional-sensitive-error", 500)
				return
			}
			replicas := 2
			if rev == headSHA && !opts.unchanged {
				replicas = 3
			}
			manifests := []string{fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d}}`, replicas)}
			if opts.workload {
				selector, desired, unavailable := "payment", 3, 0
				configValue := "before"
				if rev == headSHA {
					selector, desired, unavailable = "payments", 1, 1
					configValue = "after"
				}
				manifests = []string{
					fmt.Sprintf(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"api"},"spec":{"selector":{"app":%q},"ports":[{"port":80}]}}`, selector),
					fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":%d}},"selector":{"matchLabels":{"app":"payment"}},"template":{"metadata":{"labels":{"app":"payment"}},"spec":{"containers":[{"name":"api","image":"example.invalid/api:v1","envFrom":[{"configMapRef":{"name":"shared"}}]}]}}}}`, desired, unavailable),
					fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shared"},"data":{"setting":%q}}`, configValue),
				}
			}
			if opts.secret {
				manifests = append(manifests, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials"},"data":{"key":"++++++++"}}`)
			}
			if opts.remove && rev == headSHA {
				manifests = []string{}
			}
			// Real GetManifests responses need not contain sourceType.
			writeManifests(w, manifests)
		default:
			http.NotFound(w, r)
		}
	}})
	return s, &revisions
}

// renderedOnceEach reports whether the recorded render revisions are exactly
// want, each rendered once. Base and head render concurrently, so the order
// is not part of the contract; an extra comparison adds renders.
func renderedOnceEach(revisions []string, want ...string) bool {
	return slices.Equal(slices.Sorted(slices.Values(revisions)), slices.Sorted(slices.Values(want)))
}
