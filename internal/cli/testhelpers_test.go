package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// Shared fixtures for the CLI tests: fixed revisions, the read-only guard of
// every fake provider, the fake Git provider, a generated profile and the JSON
// diff runner. The fake Argo CD API is in
// argocdfake_test.go and the fake subprocesses are selected in main_test.go.

const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const ancestorSHA = "cccccccccccccccccccccccccccccccccccccccc"
const fakeRepo = "https://github.com/example/charts.git"

func testApp() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "api", "namespace": "argocd"},
		"spec": map[string]any{"project": "demo", "source": map[string]any{"repoURL": fakeRepo, "path": "charts/api", "targetRevision": "main", "helm": map[string]any{
			"valueFiles": []any{"values.yaml", "values-test.yaml"}, "values": "password: fictional-inline-value", "valuesObject": map[string]any{"password": "fictional-object-value"},
			"parameters": []any{map[string]any{"name": "replicas", "value": "2", "forceString": true}}, "fileParameters": []any{map[string]any{"name": "settings", "path": "settings.yaml"}}, "releaseName": "api-test",
		}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"}},
	}
}

// readOnlyFake serves handler as a fake provider API. Every request must be a
// GET carrying the bearer token; a violation fails the test and is rejected,
// so a mutation or a credential crossing an instance boundary can never look
// successful.
func readOnlyFake(t *testing.T, service, token string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("%s fake: mutation or credential crossing instance boundary: %s %s", service, r.Method, r.URL.Path)
			http.Error(w, "invalid request", http.StatusForbidden)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// githubFake describes a fake GitHub API behind readOnlyFake. routes runs
// first; returning false serves the shared routes: the main and feature
// commits, PR 12 and its merge base.
type githubFake struct {
	token  string
	routes func(http.ResponseWriter, *http.Request) bool
}

func newGitHubFake(t *testing.T, f githubFake) *httptest.Server {
	t.Helper()
	return readOnlyFake(t, "GitHub", f.token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if f.routes != nil && f.routes(w, r) {
			return
		}
		switch r.URL.Path {
		case "/repos/example/charts/commits/main", "/repos/example/charts/commits/HEAD":
			fmt.Fprintf(w, `{"sha":%q}`, baseSHA)
		case "/repos/example/charts/commits/feature":
			fmt.Fprintf(w, `{"sha":%q}`, headSHA)
		case "/repos/example/charts/pulls/12":
			fmt.Fprintf(w, `{"base":{"sha":%q,"repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, baseSHA, headSHA)
		case "/repos/example/charts/compare/" + baseSHA + "..." + headSHA:
			fmt.Fprintf(w, `{"merge_base_commit":{"sha":%q}}`, ancestorSHA)
		default:
			http.NotFound(w, r)
		}
	})
}

func gitServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newGitHubFake(t, githubFake{token: "fictional-github-token"})
}

func configFile(t *testing.T, git string, argocds ...string) string {
	t.Helper()
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	var b strings.Builder
	fmt.Fprintf(&b, "version: v1alpha1\ngit:\n  apiURL: %s\n  repository: example/charts\n  repositoryURL: %s\n  tokenEnv: DIFFARO_TEST_GITHUB\ninstances:\n", git, fakeRepo)
	for i, endpoint := range argocds {
		key := fmt.Sprintf("DIFFARO_TEST_ARGO_%d", i)
		t.Setenv(key, fmt.Sprintf("fictional-argo-%d", i))
		fmt.Fprintf(&b, "  - id: instance-%d\n    endpoint: %s\n    tokenEnv: %s\n", i, endpoint, key)
	}
	b.WriteString("applications:\n")
	for i := range argocds {
		fmt.Fprintf(&b, "  - instance: instance-%d\n    namespace: argocd\n    name: api\n", i)
	}
	filename := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(filename, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func runJSON(t *testing.T, config string, args ...string) (domain.Report, int) {
	t.Helper()
	var out, errout bytes.Buffer
	command := []string{"diff", "--config", config, "--format", "json", "--quiet"}
	command = append(command, args...)
	code := Run(context.Background(), command, &out, &errout)
	assertNoSecrets(t, "diff output", out.String()+errout.String(), "fictional-inline-value", "fictional-object-value", "fictional-sensitive-error", "fictional-argo-", "fictional-github-token")
	var report domain.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	hasSkips := false
	for _, app := range report.Applications {
		hasSkips = hasSkips || app.Execution == "skipped"
	}
	if report.Status == "success" && !hasSkips && errout.Len() != 0 {
		t.Fatal("quiet successful comparison emitted diagnostics:", errout.String())
	}
	if hasSkips && !strings.Contains(errout.String(), "analysis_skipped") {
		t.Fatal("quiet comparison hid an analysis skip")
	}
	hasIssues := len(report.Issues) > 0
	for _, app := range report.Applications {
		hasIssues = hasIssues || len(app.Issues) > 0
	}
	if report.Status != "success" && hasIssues && !strings.Contains(errout.String(), "Required action:") {
		t.Fatal("failed comparison must retain actionable stderr diagnostics with --quiet")
	}
	return report, code
}

// installedBinary returns the absolute path of the reviewed built executable
// named by DIFFARO_TEST_INSTALLED_BINARY (set by scripts/check.sh) and skips
// the test when it is unset.
func installedBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("DIFFARO_TEST_INSTALLED_BINARY")
	if binary == "" {
		t.Skip("set DIFFARO_TEST_INSTALLED_BINARY to a reviewed built executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// processExitCode returns 0 for a nil error and the exit status of a process
// that ran to completion; any other error fails the test.
func processExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatal(err)
	}
	return exited.ExitCode()
}

// runDiffaro runs the CLI in process, or the reviewed built executable named
// by DIFFARO_TEST_INSTALLED_BINARY when it is set, so the same assertions
// cover both. The executable inherits the test environment; extraEnv entries
// override inherited variables of the same name for the executable only.
func runDiffaro(t *testing.T, args []string, stdout, stderr io.Writer, extraEnv ...string) int {
	t.Helper()
	binary := os.Getenv("DIFFARO_TEST_INSTALLED_BINARY")
	if binary == "" {
		return Run(context.Background(), args, stdout, stderr)
	}
	command := exec.Command(binary, args...)
	command.Stdout, command.Stderr = stdout, stderr
	if len(extraEnv) > 0 {
		// For duplicate names exec keeps the last value.
		command.Env = append(os.Environ(), extraEnv...)
	}
	return processExitCode(t, command.Run())
}

// editProfile replaces the first occurrence of old in the profile at path.
// It fails the test when old does not occur, so a drifted anchor cannot
// silently leave the profile unchanged.
func editProfile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("profile %s does not contain %q", path, old)
	}
	if err = os.WriteFile(path, bytes.Replace(data, []byte(old), []byte(new), 1), 0600); err != nil {
		t.Fatal(err)
	}
}

// appendProfile appends text to the profile at path.
func appendProfile(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, text...), 0600); err != nil {
		t.Fatal(err)
	}
}

// assertNoSecrets fails when output contains any of secrets. Fixture
// credentials and private values all start with "fictional-", so most
// callers check that prefix.
func assertNoSecrets(t *testing.T, what, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("%s exposes %q", what, secret)
		}
	}
}
