package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// Opt-in artifact verification: run the actual extracted release executable,
// with an empty PATH and localhost mock Git/Argo servers. This does not claim
// fresh-PC OS trust or organization-specific backend/authentication coverage.
func TestInstalledBinaryFirstUse(t *testing.T) {
	binary := installedBinary(t)
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	inventory := overlayArgoCD(t, a, map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fictional-argo-0" {
			t.Error("inventory credential missing")
		}
		writeInventory(w, testApp())
	}})
	installation := t.TempDir()
	run := func(expected int, args ...string) string {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Dir = installation
		command.Env = []string{"PATH=", "DIFFARO_TEST_ARGO_0=fictional-argo-0", "DIFFARO_TEST_GITHUB=fictional-github-token"}
		for _, name := range []string{"SystemRoot", "TEMP", "TMP"} {
			if value := os.Getenv(name); value != "" {
				command.Env = append(command.Env, name+"="+value)
			}
		}
		output, err := command.CombinedOutput()
		code := processExitCode(t, err)
		if code != expected {
			t.Fatalf("installed command exit %d expected %d: %s", code, expected, output)
		}
		assertNoSecrets(t, "installed command output", string(output), "fictional-")
		return string(output)
	}
	profile := "profile with spaces.yaml"
	output := run(0, "init", "--endpoint", inventory.URL, "--pr", "https://github.com/example/charts/pull/12", "--instance", "instance-0", "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB", "--config", profile)
	// The next steps reuse the PR in doctor, diff and the profile's ci mode.
	if !strings.Contains(output, profile) || !strings.Contains(output, "Application API only") || strings.Count(output, "https://github.com/example/charts/pull/12") != 3 || !strings.Contains(output, "--mode ci --pr") {
		t.Fatal("installed init did not show the actual next commands")
	}
	// The guided short command works as shown: the init profile's ci mode
	// saves the PR Markdown and JSON reports.
	run(1, "diff", "--config", profile, "--mode", "ci", "--pr", "12")
	for _, path := range []string{"preview.md", "preview.json", "trace.json", "diffaro-bundle-private.tar.gz"} {
		if _, err := os.Stat(filepath.Join(installation, path)); err != nil {
			t.Fatal("installed ci mode did not save", path)
		}
	}
	run(1, "doctor", "--config", profile, "--render", "--pr", "12", "--quiet")
	run(1, "diff", "--config", profile, "--pr", "12", "--format", "pr-markdown", "--output", "reports/preview.md", "--json-output", "reports/preview.json", "--quiet")
	// Init saves effective credentials in a private profile; reports
	// must still never contain them.
	if info, err := os.Stat(filepath.Join(installation, profile)); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("installed init did not create a private profile")
	}
	for _, path := range []string{"reports/preview.md", "reports/preview.json"} {
		data, err := os.ReadFile(filepath.Join(installation, path))
		if err != nil {
			t.Fatal("installed flow failed to save an artifact", err)
		}
		assertNoSecrets(t, path, string(data), "fictional-")
	}
	data, err := os.ReadFile(filepath.Join(installation, "reports/preview.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report domain.Report
	if json.Unmarshal(data, &report) != nil || report.Status != "success" || !report.ChangesFound || report.Revisions.Base != ancestorSHA || report.Revisions.Head != headSHA || len(report.Applications) != 1 {
		t.Fatal("installed flow lost the comparison or machine contract")
	}
	t.Run("verbose basic child diagnostics", func(t *testing.T) {
		for _, scenario := range []string{"new", "modified", "denied"} {
			t.Run(scenario, func(t *testing.T) {
				backend, _ := recursiveServer(t, scenario, "fictional-argo-0", false, false)
				config := recursiveConfig(t, g.URL, backend.URL)
				code := 1
				if scenario == "denied" {
					code = 3
				}
				output := run(code, "diff", "--config", config, "--recursive", "--base", "main", "--head", "feature", "--format", "pr-markdown", "--output", "reports/"+scenario+".md", "--json-output", "reports/"+scenario+".json", "--quiet", "--verbose")
				for _, required := range []string{"Base: " + baseSHA, "Head: " + headSHA, "Graph path: instance-0/argocd/api -> instance-0/argocd/child", "Render backend: argocd-application-api", "Required action:"} {
					if !strings.Contains(output, required) {
						t.Fatal("installed verbose diagnostic missing:", required)
					}
				}
				reason := map[string]string{"new": "not_found (HTTP 404)", "modified": "/source/helm/releaseName", "denied": "Cause: request_forbidden"}[scenario]
				if !strings.Contains(output, reason) || strings.Contains(output, "changed-release") {
					t.Fatal("installed verbose diagnostic hid the cause or leaked an input value")
				}
				artifact, err := os.ReadFile(filepath.Join(installation, "reports", scenario+".json"))
				var compared domain.Report
				if err != nil || json.Unmarshal(artifact, &compared) != nil || compared.ExitCode() != code || len(compared.Applications) != 2 {
					t.Fatal("installed verbose lost machine output or partial results")
				}
			})
		}
	})
	t.Run("recursive project denial", func(t *testing.T) {
		denied := overlayArgoCD(t, a, map[string]http.HandlerFunc{"/api/v1/projects/demo": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("fictional-sensitive-response"))
		}})
		config := configFile(t, g.URL, denied.URL)
		output := run(5, "diff", "--config", config, "--recursive", "--base", "main", "--head", "feature", "--format", "pr-markdown", "--output", "reports/denied.md", "--json-output", "reports/denied.json", "--quiet")
		for _, text := range []string{"Credential environment: DIFFARO_TEST_ARGO_0", "system=argocd-rbac; instance=instance-0; resource=projects; action=get", "Access target: demo", "RBAC object: demo", "Side: base, head"} {
			if !strings.Contains(output, text) {
				t.Fatalf("installed project denial missing %s: %s", text, output)
			}
		}
		data, err := os.ReadFile(filepath.Join(installation, "reports/denied.json"))
		if err != nil || json.Unmarshal(data, &report) != nil || report.Status != "denied" || len(report.Applications) != 1 {
			t.Fatal("installed denial lost report boundaries")
		}
		sides := map[string]bool{}
		for _, issue := range report.Applications[0].Issues {
			access := issue.RequiredAccess
			if access == nil || access.CredentialEnv != "DIFFARO_TEST_ARGO_0" || access.Instance != "instance-0" || access.Resource != "projects" || access.Action != "get" || access.Object != "demo" {
				t.Fatal("installed denial lost the exact credential or permission")
			}
			sides[issue.Side] = true
		}
		if !sides["base"] || !sides["head"] {
			t.Fatal("installed denial lost a comparison side")
		}
		assertNoSecrets(t, "installed denial JSON", string(data), "fictional-")
		markdown, err := os.ReadFile(filepath.Join(installation, "reports/denied.md"))
		if err != nil || !bytes.Contains(markdown, []byte("RBAC object: `demo`")) {
			t.Fatal("installed denial lost the permission in the saved PR summary")
		}
		assertNoSecrets(t, "installed denial PR summary", string(markdown), "fictional-")
	})
	for _, tc := range []struct {
		name, env, reason, action, cause string
		status                           int
	}{
		{"missing", "DIFFARO_TEST_MISSING", "HTTP 401 Unauthorized", "set DIFFARO_TEST_MISSING", "credential_missing", 401},
		{"rejected", "DIFFARO_TEST_ARGO_0", "HTTP 401 Unauthorized", "expiration, revocation", "authentication_rejected", 401},
		{"forbidden", "DIFFARO_TEST_ARGO_0", "HTTP 403 Forbidden", "applications/get", "request_forbidden", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("installed binary attempted a mutation")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("fictional-sensitive-response"))
			}))
			defer denied.Close()
			output := run(5, "init", "--endpoint", denied.URL, "--pr", "https://github.com/example/charts/pull/12", "--token-env", tc.env, "--config", "denied-"+tc.name+".yaml")
			for _, text := range []string{"access denied (exit 5)", "Cause: " + tc.cause, "Application inventory lookup", tc.reason, "Credential environment: " + tc.env, "resource=applications; action=get", "Access target: selected RBAC-visible Applications", "Required action:", tc.action} {
				if !strings.Contains(output, text) {
					t.Fatalf("installed denial missing %s: %s", text, output)
				}
			}
		})
	}
}
