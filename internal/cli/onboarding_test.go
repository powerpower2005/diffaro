package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestInitDoctorAndGeneratedDiff(t *testing.T) {
	g := gitServer(t)
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	backend, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	s := overlayArgoCD(t, backend, map[string]http.HandlerFunc{
		"/api/version": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"Version":"v3.5.1+fixture"}`)) },
		"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) {
			app := testApp()
			app["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "name": "fleet", "uid": "fixture-uid", "controller": true}}
			writeInventory(w, app)
		},
	})
	for _, generated := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "generated"}[generated], func(t *testing.T) {
			t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
			t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
			path := filepath.Join(t.TempDir(), "profile.yaml")
			selection := []string{"--application", "argocd/api"}
			if generated {
				selection = []string{"--application-set", "argocd/fleet"}
			}
			args := append([]string{"init", "--endpoint", s.URL, "--instance", "test", "--config", path, "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}, selection...)
			var out, diagnostic bytes.Buffer
			if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
				t.Fatalf("init exit=%d: %s", code, diagnostic.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertNoSecrets(t, "init profile", string(data), "fictional-inline-value", "fictional-object-value")
			assertNoSecrets(t, "init output", out.String(), "fictional-inline-value", "fictional-object-value")
			if _, err = profile.Load(path); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			diagnostic.Reset()
			if Run(context.Background(), args, &out, &diagnostic) != 4 {
				t.Fatal("init overwrote an existing file")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(data, after) {
				t.Fatal("existing profile changed")
			}
			t.Setenv("DIFFARO_TEST_ARGO_0", "")
			t.Setenv("DIFFARO_TEST_GITHUB", "")
			report, code := runJSON(t, path, "--base", "main", "--head", "feature")
			if code != 1 || len(report.Applications) != 1 || report.Applications[0].State != "changed" || (len(report.Selections) == 1) != generated {
				t.Fatal("generated profile did not produce a scoped target diff")
			}
			out.Reset()
			diagnostic.Reset()
			code = Run(context.Background(), []string{"doctor", "--config", path, "--render", "--format", "json"}, &out, &diagnostic)
			if code != 0 || json.Unmarshal(out.Bytes(), &report) != nil || report.Revisions.Base != baseSHA || report.Revisions.Head != baseSHA || len(report.Applications[0].Resources) == 0 || report.RenderBackends[0].Compatibility != "version-in-tested-matrix" || report.RenderBackends[0].Verification != "live-unverified" {
				t.Fatalf("render preflight lost evidence or overstated verification (exit %d)", code)
			}
		})
	}
}

func TestDoctorRejectsUnsupportedSourceBeforeRendering(t *testing.T) {
	g := gitServer(t)
	a, revisions := argoServer(t, "fictional-argo-0", backendOptions{unsupported: true})
	path := configFile(t, g.URL, a.URL)
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--config", path, "--format", "json"}, &out, &errout)
	var report domain.Report
	if code != 6 || json.Unmarshal(out.Bytes(), &report) != nil || report.Applications[0].Issues[0].Code != "unsupported_source" || len(*revisions) != 0 {
		t.Fatal("doctor accepted unsupported renderer or executed it")
	}
}

func TestInitOrdinaryApplicationAsAppOfAppsRoot(t *testing.T) {
	for _, scenario := range []string{"normal", "new"} {
		t.Run(scenario, func(t *testing.T) {
			g := gitServer(t)
			t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
			t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
			backend, calls := recursiveServer(t, scenario, "fictional-argo-0", false, false)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/applications" || strings.HasPrefix(r.URL.Path, "/api/v1/applicationsets") {
					t.Error("ordinary App-of-Apps unexpectedly requires inventory or ApplicationSet lookup")
					http.Error(w, "unexpected selection", 500)
					return
				}
				if r.URL.Path == "/api/version" {
					_, _ = w.Write([]byte(`{"Version":"v3.5.1"}`))
					return
				}
				backend.Config.Handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			path := filepath.Join(t.TempDir(), "root.yaml")
			var out, diagnostic bytes.Buffer
			code := Run(context.Background(), []string{"init", "--endpoint", s.URL, "--instance", "test", "--application", "argocd/api", "--single-instance", "--config", path, "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}, &out, &diagnostic)
			if code != 0 {
				t.Fatalf("init failed: %s", diagnostic.String())
			}
			config, err := profile.Load(path)
			if err != nil || len(config.Applications) != 1 || len(config.ApplicationSets) != 0 || config.Routing.DefaultInstance != "test" {
				t.Fatal("ordinary root was misclassified or explicit routing was lost")
			}
			out.Reset()
			diagnostic.Reset()
			code = Run(context.Background(), []string{"doctor", "--config", path, "--render", "--recursive", "--format", "json"}, &out, &diagnostic)
			var diagnosis domain.Report
			if code != 0 || json.Unmarshal(out.Bytes(), &diagnosis) != nil || diagnosis.Graph == nil || !diagnosis.Graph.HeadComplete || len(diagnosis.Selections) != 0 {
				t.Fatal("ordinary App-of-Apps preflight failed or invented ApplicationSet provenance")
			}
			calls.Store(0)
			t.Setenv("DIFFARO_TEST_ARGO_0", "")
			t.Setenv("DIFFARO_TEST_GITHUB", "")
			report, code := runJSON(t, path, "--recursive", "--base", "main", "--head", "feature")
			if report.Graph == nil || len(report.Applications) != 2 || len(report.Selections) != 0 {
				t.Fatal("ordinary Application children were not discovered from rendered targets")
			}
			if scenario == "normal" {
				if code != 1 || calls.Load() != 2 || !report.Graph.HeadComplete {
					t.Fatal("child was not rendered at both target revisions")
				}
			} else {
				found := false
				for _, app := range report.Applications {
					if app.ID.Name == "child" && app.Execution == "skipped" && app.SkipReason != "" && len(app.Resources) == 0 && len(app.Issues) == 0 {
						found = true
					}
				}
				if code != 1 || !found {
					t.Fatal("new child without detailed backend did not expose skipped analysis")
				}
			}
		})
	}
}
