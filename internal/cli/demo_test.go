package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestDemoCLIOutputsAndInputValidation(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("KUBECONFIG", "must-not-be-read")
	path := filepath.Join(t.TempDir(), "preview.json")
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"demo", "--json-output", path}, &out, &diagnostic); code != 1 {
		t.Fatal(code, diagnostic.String())
	}
	for _, want := range []string{"DEMO fixture (offline)", "[+] App demo/argocd/worker", "Deployment/worker", "Service/worker", "ConfigMap/worker"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("human demo missing", want)
		}
	}
	data, err := os.ReadFile(path)
	var report domain.Report
	if err != nil || json.Unmarshal(data, &report) != nil || report.Revisions.Mode != "demo-fixture-base-head" {
		t.Fatal("machine demo is invalid", err)
	}
	out.Reset()
	diagnostic.Reset()
	if code := Run(context.Background(), []string{"demo", "--format", "json"}, &out, &diagnostic); code != 1 || !bytes.Equal(data, out.Bytes()) {
		t.Fatal("paired demo reports use different comparisons", code)
	}
	out.Reset()
	diagnostic.Reset()
	if code := Run(context.Background(), []string{"demo", "--format", "markdown", "--json-output", path}, &out, &diagnostic); code != 1 || !strings.HasPrefix(out.String(), "> **Offline demo fixture:**") {
		t.Fatal("Markdown lost demo provenance", code)
	}
	markdownJSON, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, markdownJSON) {
		t.Fatal("Markdown changed machine comparison")
	}
	// PR Markdown previews the PR comment but carries no integration marker
	// or sample SHAs, so it cannot be taken for a PR result.
	out.Reset()
	diagnostic.Reset()
	if code := Run(context.Background(), []string{"demo", "--format", "pr-markdown", "--json-output", path}, &out, &diagnostic); code != 1 || !strings.HasPrefix(out.String(), "> **Offline demo fixture:**") || !strings.Contains(out.String(), "## Diffaro:") {
		t.Fatalf("PR Markdown demo lost provenance or content: %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "kubeseer:preview") || strings.Contains(out.String(), "<!-- Base:") {
		t.Fatalf("PR Markdown demo carries the integration marker:\n%s", out.String())
	}
	if prJSON, err := os.ReadFile(path); err != nil || !bytes.Equal(data, prJSON) {
		t.Fatal("PR Markdown changed machine comparison")
	}
	for _, args := range [][]string{{"demo", "--scenario", "other"}, {"demo", "--format", "html"}, {"demo", "--format", "json", "--json-output", path}, {"demo", "--output", path, "--json-output", path}, {"demo", "--config", "missing.yaml"}, {"demo", "extra"}} {
		out.Reset()
		diagnostic.Reset()
		if code := Run(context.Background(), args, &out, &diagnostic); code != 4 || out.Len() != 0 {
			t.Fatal("invalid demo input accepted", args, code)
		}
	}
	out.Reset()
	diagnostic.Reset()
	if code := Run(context.Background(), []string{"demo", "--help"}, &out, &diagnostic); code != 0 || !strings.Contains(strings.ToLower(out.String()), "offline") {
		t.Fatal("demo help requires config or misses boundary")
	}
}

type demoClosedWriter struct{}

func (demoClosedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDemoOutputFailureAndCancellation(t *testing.T) {
	var diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"demo"}, demoClosedWriter{}, &diagnostic); code != 6 || !strings.Contains(diagnostic.String(), "cannot write") {
		t.Fatal("stdout failure hidden", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	diagnostic.Reset()
	code := Run(ctx, []string{"demo", "--format", "json"}, &out, &diagnostic)
	var report domain.Report
	if code != 7 || json.Unmarshal(out.Bytes(), &report) != nil || report.Status != "cancelled" || report.Revisions.Mode != "demo-fixture-base-head" {
		t.Fatal("CLI cancellation lost structured result", code)
	}
}

// TestDiffStdoutFailureIsFailure checks that diff, which shares report
// delivery with demo, reports an unwritable stdout as a failure.
func TestDiffStdoutFailureIsFailure(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	var diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"diff", "--config", configFile(t, g.URL, a.URL), "--base", "main", "--head", "feature", "--quiet"}, demoClosedWriter{}, &diagnostic); code != 6 {
		t.Fatal("stdout failure reported as a completed comparison", code, diagnostic.String())
	}
}

func TestInstalledBinaryOfflineDemo(t *testing.T) {
	binary := installedBinary(t)
	dir := t.TempDir()
	for _, scenario := range []string{"new-app", "partial-failure", "skipped-context"} {
		command := exec.Command(binary, "demo", "--scenario", scenario, "--format", "terminal", "--json-output", scenario+".json")
		command.Dir = dir
		command.Env = []string{"PATH=", "KUBECONFIG=does-not-exist", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1"}
		for _, name := range []string{"SystemRoot", "TEMP", "TMP"} {
			if value := os.Getenv(name); value != "" {
				command.Env = append(command.Env, name+"="+value)
			}
		}
		output, err := command.CombinedOutput()
		var failure *exec.ExitError
		if !errors.As(err, &failure) {
			t.Fatal("expected changes/failure exit", err)
		}
		want := 1
		if scenario == "partial-failure" {
			want = 3
		}
		if failure.ExitCode() != want || !bytes.Contains(output, []byte("DEMO fixture (offline)")) {
			t.Fatal("installed offline demo failed", string(output))
		}
		data, err := os.ReadFile(filepath.Join(dir, scenario+".json"))
		var report domain.Report
		if err != nil || json.Unmarshal(data, &report) != nil || report.ExitCode() != want {
			t.Fatal("installed demo output lost", err)
		}
		if scenario == "new-app" && !bytes.Contains(output, []byte("Deployment/worker")) {
			t.Fatal("new child targets missing")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatal("demo created unexpected profile/cache files", entries, err)
	}
}

func TestDemoScenarioExitCodes(t *testing.T) {
	for _, scenario := range []string{"new-app", "partial-failure", "skipped-context"} {
		for _, format := range []string{"terminal", "json"} {
			var out, diagnostic bytes.Buffer
			code := Run(context.Background(), []string{"demo", "--scenario", scenario, "--format", format}, &out, &diagnostic)
			want := 1
			if scenario == "partial-failure" {
				want = 3
			}
			if code != want || out.Len() == 0 {
				t.Fatal("demo scenario changed exit behavior", scenario, format, code)
			}
		}
	}
}
