package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

const runSection = `run:
  defaults:
    format: terminal
  modes:
    ci:
      format: pr-markdown
      output: preview.md
      jsonOutput: preview.json
      trace: trace.json
      debugBundle: bundle.tar.gz
      debugBundleLevel: private
      quiet: true
`

// withRun appends a run section to a test profile and moves into a fresh
// directory, where relative output paths land.
func withRun(t *testing.T, config, run string) string {
	t.Helper()
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(data, []byte(run)...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	return config
}

func runCommand(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// --mode applies the profile's mode over its defaults, so a CI command needs
// only its revisions, and the report names the mode.
func TestProfileModeSuppliesTheOptions(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := withRun(t, configFile(t, git.URL, argo.URL), runSection)
	code, stdout, stderr := runCommand("diff", "--config", config, "--mode", "ci", "--base", "main", "--head", "feature")
	if code != domain.ExitChangesFound || stdout != "" || stderr != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	markdown, _ := os.ReadFile("preview.md")
	if !bytes.HasPrefix(markdown, []byte("<!-- kubeseer:preview -->")) {
		t.Fatalf("output is not PR Markdown: %.80s", markdown)
	}
	var report domain.Report
	data, _ := os.ReadFile("preview.json")
	if err := json.Unmarshal(data, &report); err != nil || report.Generator == nil || report.Generator.Mode != "ci" {
		t.Fatalf("json output: %v %+v", err, report.Generator)
	}
	if !exists("trace.json") || !exists("bundle.tar.gz") || exists("bundle.tar.gz.names.json") {
		t.Fatal("mode trace or private bundle missing")
	}
}

// Without --mode only run.defaults apply; explicit flags beat both layers.
func TestFlagsOverrideProfileDefaultsAndModes(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := withRun(t, configFile(t, git.URL, argo.URL), runSection)
	code, stdout, _ := runCommand("diff", "--config", config, "--base", "main", "--head", "feature", "--quiet")
	if code != domain.ExitChangesFound || !strings.Contains(stdout, "[~]") || strings.HasPrefix(stdout, "<!--") || exists("preview.md") {
		t.Fatalf("defaults did not select terminal output: code %d\n%s", code, stdout)
	}
	code, stdout, stderr := runCommand("diff", "--config", config, "--mode", "ci", "--base", "main", "--head", "feature", "--format", "json", "--output", "flag.json")
	if code != domain.ExitChangesFound || stdout != "" {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	data, _ := os.ReadFile("flag.json")
	if !json.Valid(data) || exists("preview.md") || exists("preview.json") {
		t.Fatal("flags did not override the mode's format and outputs")
	}
	if !exists("trace.json") {
		t.Fatal("mode options without a flag were not applied")
	}
}

// A profile that makes runs recursive, as init's does, still lets one run
// compare only an explicit root's own targets with --recursive=false.
func TestRecursiveFalseOverridesRecursiveProfileDefault(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := withRun(t, configFile(t, git.URL, argo.URL), "run:\n  defaults:\n    recursive: true\n")
	for _, tc := range []struct {
		args     []string
		coverage string
	}{
		{nil, "recursive-target-graph"},
		{[]string{"--recursive=false"}, "explicit-applications"},
	} {
		report, code := runJSON(t, config, append([]string{"--base", "main", "--head", "feature"}, tc.args...)...)
		if code != domain.ExitChangesFound || report.Coverage != tc.coverage {
			t.Fatalf("%v: code %d coverage %q, want %q", tc.args, code, report.Coverage, tc.coverage)
		}
	}
}

// An undefined mode is a configuration error that names the defined modes.
func TestUndefinedModeIsRejected(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := withRun(t, configFile(t, git.URL, argo.URL), runSection)
	code, _, stderr := runCommand("diff", "--config", config, "--mode", "nightly", "--base", "main", "--head", "feature")
	if code != domain.ExitInvalidConfiguration || !strings.Contains(stderr, `mode "nightly" is not defined`) || !strings.Contains(stderr, "defined: ci") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}

// doctor takes the options it supports from a mode and leaves the diff-only
// ones, so one profile serves both commands.
func TestDoctorIgnoresDiffOnlyModeOptions(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := withRun(t, configFile(t, git.URL, argo.URL), runSection)
	code, _, stderr := runCommand("doctor", "--config", config, "--mode", "ci")
	if code != domain.ExitSuccess {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	data, _ := os.ReadFile("preview.md")
	if len(data) == 0 || bytes.HasPrefix(data, []byte("<!--")) || exists("trace.json") || exists("bundle.tar.gz") || exists("preview.json") {
		t.Fatalf("doctor applied diff-only options: %.80s", data)
	}
}

// validate checks the defaults and every mode with the flag rules.
func TestValidateChecksEveryRunMode(t *testing.T) {
	for name, run := range map[string]string{
		"run.modes.bad":  "run:\n  modes:\n    bad:\n      format: yaml\n",
		"run.defaults":   "run:\n  defaults:\n    debugBundleLevel: private\n",
		"run.modes.same": "run:\n  modes:\n    same:\n      output: report.md\n      jsonOutput: report.md\n      format: markdown\n",
		"run.modes":      "run:\n  modes:\n    Bad_Name: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			config := withRun(t, configFile(t, "https://api.github.example.invalid", "https://argocd.example.invalid"), run)
			code, _, stderr := runCommand("validate", "--config", config)
			if code != domain.ExitInvalidConfiguration || !strings.Contains(stderr, name) {
				t.Fatalf("code %d stderr %s", code, stderr)
			}
		})
	}
	config := withRun(t, configFile(t, "https://api.github.example.invalid", "https://argocd.example.invalid"), runSection)
	code, stdout, stderr := runCommand("validate", "--config", config)
	if code != domain.ExitSuccess {
		t.Fatalf("valid run section rejected: %s", stderr)
	}
	// validate shows what each short command will do.
	for _, want := range []string{
		"Run defaults (diffaro diff): format terminal, report to stdout",
		"Run mode ci (diffaro diff --mode ci): format pr-markdown, output preview.md, json-output preview.json, trace trace.json, debug-bundle bundle.tar.gz (private), quiet",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output lacks %q:\n%s", want, stdout)
		}
	}
	plain := configFile(t, "https://api.github.example.invalid", "https://argocd.example.invalid")
	if _, stdout, _ := runCommand("validate", "--config", plain); strings.Contains(stdout, "Run ") {
		t.Fatalf("validate describes run settings the profile does not have:\n%s", stdout)
	}
}

// Without --config, diffaro.yaml is read, or diffaro.local.yaml, which init
// writes, when diffaro.yaml does not exist.
func TestProfileLookupFallsBackToTheInitProfile(t *testing.T) {
	valid, err := os.ReadFile(configFile(t, "https://api.github.example.invalid", "https://argocd.example.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if code, _, stderr := runCommand("validate"); code != domain.ExitInvalidConfiguration || !strings.Contains(stderr, "no profile found") || !strings.Contains(stderr, "diffaro init") {
		t.Fatalf("missing profile: code %d stderr %s", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(".", initConfig), valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand("validate"); code != domain.ExitSuccess {
		t.Fatalf("init profile not found: %s", stderr)
	}
	if err := os.WriteFile(defaultConfig, []byte("version: v0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCommand("validate"); code != domain.ExitInvalidConfiguration {
		t.Fatal("diffaro.yaml must be preferred over diffaro.local.yaml")
	}
}
