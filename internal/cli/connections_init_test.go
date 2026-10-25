package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestCatalogInitToDoctorAndMultiRepositoryPRArtifacts(t *testing.T) {
	// Optional verification of the built executable uses the same assertions,
	// localhost servers and saved artifacts with no external command dependency
	// (empty PATH).
	run := func(args []string, stdout, stderr io.Writer) int {
		return runDiffaro(t, args, stdout, stderr, "PATH=")
	}
	git := gitServer(t)
	var requests atomic.Int32
	endpoints := []string{}
	for i := 0; i < 2; i++ {
		backend, _ := multiInstanceServer(t, i, "multi-repo")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.URL.Path != "/api/v1/applications" {
				backend.Config.Handler.ServeHTTP(w, r)
				return
			}
			if r.Method != "GET" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer fictional-argo-%d", i) {
				t.Error("bootstrap inventory crossed instance token or mutated state")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{multiInstanceApp("api")}})
		}))
		t.Cleanup(server.Close)
		endpoints = append(endpoints, server.URL)
	}
	_ = configFile(t, git.URL, endpoints...)
	dir := t.TempDir()
	catalog := filepath.Join(dir, "connections.yaml")
	contents := fmt.Sprintf("instances:\n  - id: instance-0\n    endpoint: %s\n    tokenEnv: DIFFARO_TEST_ARGO_0\n  - id: instance-1\n    endpoint: %s\n    tokenEnv: DIFFARO_TEST_ARGO_1\n", endpoints[0], endpoints[1])
	if err := os.WriteFile(catalog, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "profile with spaces.yaml")
	var out, diagnostic bytes.Buffer
	pr := "https://github.com/example/charts/pull/12"
	args := []string{"init", "--connections", catalog, "--pr", pr, "--local-children", "--config", configPath, "--git-api", git.URL, "--git-token-env", "DIFFARO_TEST_GITHUB"}
	if code := run(args, &out, &diagnostic); code != 0 || requests.Load() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("offline multi-instance setup requires connectivity: exit=%d %s", code, diagnostic.String())
	}
	for _, text := range []string{"Created offline", "DIFFARO_TEST_ARGO_0", "DIFFARO_TEST_ARGO_1", "--mode ci", "recursive terminal report", pr} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("bootstrap guidance omitted %s", text)
		}
	}
	assertNoSecrets(t, "bootstrap guidance", out.String(), "fictional-")
	c, err := profile.Load(configPath)
	if err != nil || c.Discovery == nil || len(c.Instances) != 2 || len(c.Routing.Clusters) != 4 || c.Routing.DefaultInstance != "" {
		t.Fatal("bootstrap did not produce explicit scoped discovery")
	}
	assertStarterRun(t, c.Run)
	if c.Discovery.Selection != profile.SelectionChangedPaths {
		t.Fatalf("init --pr selection = %q, want the changedPaths default", c.Discovery.Selection)
	}
	before, _ := os.ReadFile(configPath)
	if code := run(args, &out, &diagnostic); code != 4 {
		t.Fatal("catalog init overwrote an existing profile")
	}
	after, _ := os.ReadFile(configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("existing profile changed")
	}
	t.Setenv("DIFFARO_TEST_ARGO_0", "")
	t.Setenv("DIFFARO_TEST_ARGO_1", "")
	t.Setenv("DIFFARO_TEST_GITHUB", "")
	out.Reset()
	diagnostic.Reset()
	if code := run([]string{"doctor", "--config", configPath, "--render", "--recursive", "--pr", pr, "--format", "json", "--quiet"}, &out, &diagnostic); code != 1 {
		t.Fatalf("generated profile failed rendered doctor: %d %s", code, diagnostic.String())
	}
	markdown, machine := filepath.Join(dir, "preview.md"), filepath.Join(dir, "preview.json")
	out.Reset()
	diagnostic.Reset()
	if code := run([]string{"diff", "--config", configPath, "--recursive", "--pr", pr, "--format", "pr-markdown", "--output", markdown, "--json-output", machine, "--quiet"}, &out, &diagnostic); code != 1 || out.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("generated profile failed PR artifacts: %d %s", code, diagnostic.String())
	}
	jsonData, err := os.ReadFile(machine)
	var report domain.Report
	if err != nil || json.Unmarshal(jsonData, &report) != nil || len(report.Applications) != 8 || report.Revisions.Base != ancestorSHA || report.Revisions.Head != headSHA || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
		t.Fatal("multi-repository bootstrap did not produce complete target graphs and resolved PR revisions")
	}
	md, err := os.ReadFile(markdown)
	if err != nil || !bytes.Contains(md, []byte(headSHA)) || !bytes.Contains(md, []byte("<!-- kubeseer:preview -->")) {
		t.Fatal("PR artifact lost its head SHA/marker")
	}
	assertNoSecrets(t, "PR artifacts", string(md)+string(jsonData), "fictional-")
}

func TestThreeManagerFirstUseCatalog(t *testing.T) {
	catalog := filepath.Join(t.TempDir(), "connections.yaml")
	if err := os.WriteFile(catalog, []byte(`# Trusted operator configuration. Token values belong in the secret store.
instances:
  - id: root
    endpoint: https://argocd-root.example.invalid
    tokenEnv: DIFFARO_ARGOCD_ROOT_TOKEN
    localHelm: true
  - id: east
    endpoint: https://argocd-east.example.invalid
    tokenEnv: DIFFARO_ARGOCD_EAST_TOKEN
    localHelm: true
  - id: west
    endpoint: https://argocd-west.example.invalid
    tokenEnv: DIFFARO_ARGOCD_WEST_TOKEN
    localHelm: true
# Routing identifies the manager for Application CRs rendered by a parent.
# It does not transfer ownership of installation Deployments or workloads.
# Adapt these routes to your graph; an installation relation alone is insufficient.
routing:
  clusters:
    - parentInstance: root
      cluster: root-cluster
      namespace: argocd
      instance: root
    - parentInstance: root
      cluster: central-east
      namespace: argocd
      instance: east
    - parentInstance: root
      cluster: central-west
      namespace: argocd
      instance: west
    - parentInstance: east
      cluster: central-east
      namespace: argocd
      instance: east
    - parentInstance: west
      cluster: central-west
      namespace: argocd
      instance: west
`), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "profile.yaml")
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"init", "--connections", catalog, "--pr", "https://github.com/example/charts/pull/123", "--local-children", "--local-helm", "--config", configPath}, &out, &diagnostic)
	if code != 0 {
		t.Fatal("offline catalog setup failed", code, diagnostic.String())
	}
	c, err := profile.Load(configPath)
	if err != nil || len(c.Instances) != 3 || len(c.Routing.Clusters) != 11 || c.Routing.DefaultInstance != "" {
		t.Fatal("manager routing/catalog invalid", err)
	}
	// One line names every instance's token variable and the Git one once.
	if lines := strings.Count(out.String(), "Credential fallback environment variables:"); lines != 1 || strings.Count(out.String(), "DIFFARO_GITHUB_TOKEN") != 1 {
		t.Fatalf("credential variables repeated per instance:\n%s", out.String())
	}
	for _, instance := range c.Instances {
		if !instance.LocalHelm || instance.RepoServer != nil || !strings.Contains(out.String(), instance.TokenEnv) {
			t.Fatal("manager backend/credentials missing", instance.ID)
		}
		parent := domain.Application{ID: domain.ApplicationID{Instance: instance.ID, Namespace: "argocd", Name: "parent"}, Spec: map[string]any{"destination": map[string]any{"name": "in-cluster", "namespace": "argocd"}}}
		child := domain.Application{ID: domain.ApplicationID{Namespace: "argocd", Name: "child"}}
		id, _, err := c.Routing.Route(parent, child)
		if err != nil || id != instance.ID {
			t.Fatal("in-cluster route crossed manager", instance.ID, id, err)
		}
	}
}

func TestCatalogInitRejectsMixedInputsWithoutWriting(t *testing.T) {
	for _, options := range [][]string{
		{}, {"--pr", "https://github.com/example/charts/pull/12", "--endpoint", "https://dev.example.com"},
		{"--pr", "https://github.com/example/charts/pull/12", "--single-instance"},
		{"--pr", "https://github.com/example/charts/pull/12", "--child-namespace", "team-apps"},
		{"--pr", "https://github.com/example/charts/pull/12", "--child-namespace", "argocd"},
		{"--pr", "https://github.com/example/charts/pull/12", "--local-children", "--child-namespace", "../unsafe"},
	} {
		dir := t.TempDir()
		catalog, output := filepath.Join(dir, "connections.yaml"), filepath.Join(dir, "profile.yaml")
		if err := os.WriteFile(catalog, []byte("instances:\n  - id: dev\n    endpoint: https://dev.example.com\n    tokenEnv: DEV_TOKEN\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"init", "--connections", catalog, "--config", output}, options...)
		var out, diagnostic bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostic); code != 4 {
			t.Fatalf("invalid setup accepted: %v", options)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("invalid input left a profile behind")
		}
	}
}
