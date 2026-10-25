package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"diffaro/internal/profile"
)

func TestInitPrintsUsableNextCommandsAndConnectionBoundary(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	path := filepath.Join(t.TempDir(), "profile with spaces.yaml")
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"init", "--endpoint", a.URL, "--application", "argocd/api", "--config", path, "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}, &out, &diagnostic)
	if code != 0 {
		t.Fatalf("init failed: %d %s", code, diagnostic.String())
	}
	// The written run section supplies recursion, terminal output and the
	// ci mode's saved JSON, so the next steps name the mode.
	for _, want := range []string{path, "diffaro doctor --config", "--mode ci --pr", "preview.json", "Application API only", "New or reconfigured child", "skipped with an explicit reason", "optional detailed analysis", "DIFFARO_TEST_ARGO_0", "Exit 1 means changes found", "GitHub API: " + g.URL, "recommended even for public repositories", "60 unauthenticated API requests per hour"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("next steps miss %q", want)
		}
	}
	assertNoSecrets(t, "command guidance", out.String(), "fictional-")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := profile.Parse(data)
	if err != nil || c.Git.Token != "fictional-github-token" || c.Instances[0].Token != "fictional-argo-0" {
		t.Fatal("profile did not save credentials")
	}
	assertStarterRun(t, c.Run)
}

// assertStarterRun checks that an init profile makes diff a recursive
// terminal report and has a ci mode that saves PR Markdown and JSON. Its
// private debug bundle has a file name that says so, so it is not mistaken
// for the public bundle a bug report attaches.
func assertStarterRun(t *testing.T, run profile.Run) {
	t.Helper()
	ci, ok := run.Modes["ci"]
	if run.Defaults.Format != "terminal" || run.Defaults.Recursive == nil || !*run.Defaults.Recursive || !ok || ci.Format != "pr-markdown" || ci.JSONOutput != "preview.json" || ci.Output != "preview.md" {
		t.Fatalf("init profile run section = %+v", run)
	}
	if ci.DebugBundleLevel != "private" || !strings.Contains(ci.DebugBundle, "private") {
		t.Fatalf("init ci mode private bundle %q at level %q does not name its level", ci.DebugBundle, ci.DebugBundleLevel)
	}
}

func TestInitGuidanceReusesPRAndDistinguishesRepoServer(t *testing.T) {
	c := profile.Config{Git: profile.Git{TokenEnv: "DIFFARO_GITHUB_TOKEN"}, Instances: []profile.Instance{{TokenEnv: "DIFFARO_ARGOCD_TOKEN", RepoServer: &profile.RepoServer{}}}}
	var out bytes.Buffer
	writeInitNextSteps(&out, c, "profile.yaml", "https://github.com/example/charts/pull/123")
	if strings.Count(out.String(), "https://github.com/example/charts/pull/123") != 2 || strings.Contains(out.String(), "PR_NUMBER_OR_URL") || strings.Contains(out.String(), "Application API only") || !strings.Contains(out.String(), "repo-server configured") {
		t.Fatal("PR next steps lost the selected comparison or render setup")
	}
}

// The next commands leave out --config only for the profile that commands
// read without it: diffaro.yaml, or diffaro.local.yaml while no diffaro.yaml
// exists. A diffaro.yaml that shadows the new profile is named.
func TestInitNextCommandsKeepConfigWhenDiffaroYAMLShadowsIt(t *testing.T) {
	recursive := true
	c := profile.Config{Git: profile.Git{TokenEnv: "DIFFARO_GITHUB_TOKEN"}, Instances: []profile.Instance{{TokenEnv: "DIFFARO_ARGOCD_TOKEN", LocalHelm: true}},
		Run: profile.Run{Defaults: profile.RunOptions{Format: "terminal", Recursive: &recursive}, Modes: map[string]profile.RunOptions{"ci": {Format: "pr-markdown"}}}}
	t.Chdir(t.TempDir())
	next := func(path string) string {
		var out bytes.Buffer
		writeInitNextSteps(&out, c, path, "https://github.com/example/charts/pull/123")
		return out.String()
	}
	if out := next(initConfig); !strings.Contains(out, "diffaro doctor --render --pr") || strings.Contains(out, "--config") || strings.Contains(out, "takes precedence") {
		t.Fatalf("the profile read by default got --config or a warning:\n%s", out)
	}
	if err := os.WriteFile(defaultConfig, []byte("version: v1alpha1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := next(initConfig); !strings.Contains(out, "diffaro doctor --config "+shellArgument(initConfig, runtime.GOOS)+" --render") || !strings.Contains(out, "diffaro diff --config "+shellArgument(initConfig, runtime.GOOS)+" --pr") || !strings.Contains(out, "diffaro.yaml in this directory takes precedence over diffaro.local.yaml") {
		t.Fatalf("a shadowed profile lost --config or the warning:\n%s", out)
	}
	if out := next(defaultConfig); strings.Contains(out, "--config") || strings.Contains(out, "takes precedence") {
		t.Fatalf("diffaro.yaml got --config or a warning:\n%s", out)
	}
}

func TestDisplayedCommandsQuoteShellExpansion(t *testing.T) {
	value := "profile's $(command) `command` $TOKEN.yaml"
	if shellArgument(value, "windows") != "'profile''s $(command) `command` $TOKEN.yaml'" || shellArgument(value, "linux") != "'profile'\\''s $(command) `command` $TOKEN.yaml'" {
		t.Fatal("copyable command permits shell expansion")
	}
}

func TestExplicitRootGuidanceIncludesChildrenAndTerminal(t *testing.T) {
	c := profile.Config{Git: profile.Git{TokenEnv: "DIFFARO_GITHUB_TOKEN"}, Instances: []profile.Instance{{TokenEnv: "DIFFARO_ARGOCD_TOKEN", LocalHelm: true}}}
	var out bytes.Buffer
	writeInitNextSteps(&out, c, "profile.yaml", "https://github.com/example/charts/pull/123")
	for _, want := range []string{"--render --pr", "--recursive --format terminal", "--json-output preview.json", "Reuse this profile", "--format pr-markdown"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("explicit-root first-use guidance missing", want, out.String())
		}
	}
}

// An explicit Application whose source already reads through a host makes
// that host git.repositoryURL's; a --mirror-host naming it needs no entry and
// is reported instead of failing init, while other mirror hosts are kept.
func TestInitSkipsMirrorHostThatIsTheRepositoryHost(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	path := filepath.Join(t.TempDir(), "profile.yaml")
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"init", "--endpoint", a.URL, "--application", "argocd/api", "--config", path, "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--mirror-host", "https://github.com", "--mirror-host", "https://mirror.example.com"}, &out, &diagnostic)
	if code != 0 {
		t.Fatalf("init failed: %d %s", code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "--mirror-host https://github.com is already the repository host") {
		t.Fatalf("skipped mirror host not reported:\n%s", out.String())
	}
	c, err := profile.Load(path)
	if err != nil || len(c.Git.MirrorHosts) != 1 || c.Git.MirrorHosts[0] != "https://mirror.example.com" {
		t.Fatalf("mirror hosts = %v, err %v", c.Git.MirrorHosts, err)
	}
}

// init names the two settings a large repository or a custom Helm build
// usually needs, so a first run neither renders every candidate nor fails
// every custom function (found in a live run).
func TestInitGuidanceNamesSelectionAndHelmEnvironment(t *testing.T) {
	c := profile.Config{Git: profile.Git{TokenEnv: "DIFFARO_GITHUB_TOKEN"}, Discovery: &profile.Discovery{Mode: "repository"}, Instances: []profile.Instance{{TokenEnv: "DIFFARO_ARGOCD_TOKEN", LocalHelm: true, HelmBinary: "/opt/fictional/helm"}}}
	var out bytes.Buffer
	writeInitNextSteps(&out, c, "profile.yaml", "")
	if !strings.Contains(out.String(), "discovery.selection: changedPaths") || !strings.Contains(out.String(), "--helm-env-from NAME=ENV") {
		t.Fatalf("guidance lacks the selection or Helm environment hint:\n%s", out.String())
	}
	c.Discovery.Selection = profile.SelectionChangedPaths
	c.Instances[0].HelmEnvironment = []profile.HelmEnvVar{{Name: "FICTIONAL_ENDPOINT", Value: "x"}}
	out.Reset()
	writeInitNextSteps(&out, c, "profile.yaml", "")
	if strings.Contains(out.String(), "discovery.selection: changedPaths") || strings.Contains(out.String(), "--helm-env-from NAME=ENV") {
		t.Fatalf("guidance repeats hints for settings already made:\n%s", out.String())
	}
}
