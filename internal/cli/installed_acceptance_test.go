package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
)

// These cases exercise the installed process, not Run or a replacement renderer.
// Backend fixtures assert read-only calls, immutable revisions and token isolation.
// They complement actual Argo CD acceptance runs; mock manifests are not Helm proof.
func TestInstalledBinaryAcceptance(t *testing.T) {
	binary := installedBinary(t)
	run := func(t *testing.T, args ...string) ([]byte, []byte, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "PATH=")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var failure *exec.ExitError
			if !errors.As(err, &failure) || ctx.Err() != nil {
				t.Fatal("installed process could not complete within its bound")
			}
			code = failure.ExitCode()
		}
		secrets := []string{"fictional-github-token", "fictional-argo-", "fictional-inline-value", "fictional-object-value", "fictional-repository-secret", "fictional-sensitive-error", "++++++++"}
		assertNoSecrets(t, "installed process stdout", stdout.String(), secrets...)
		assertNoSecrets(t, "installed process stderr", stderr.String(), secrets...)
		return stdout.Bytes(), stderr.Bytes(), code
	}
	for _, tc := range []struct {
		name                     string
		opts                     backendOptions
		code                     int
		state                    string
		policy, secondDenied, pr bool
	}{
		{name: "explicit-references", code: 1, state: "changed"},
		{name: "pr-merge-base", code: 1, state: "changed", pr: true},
		{name: "no-change", opts: backendOptions{unchanged: true}, state: "unchanged"},
		{name: "permission-denial", opts: backendOptions{denied: true}, code: 5, state: "denied"},
		{name: "head-render-failure", opts: backendOptions{headFailure: true}, code: 6, state: "failed"},
		{name: "render-context-drift", opts: backendOptions{drift: true}, code: 6, state: "failed"},
		{name: "masked-secret-unknown", opts: backendOptions{secret: true}, code: 1, state: "changed"},
		{name: "unsupported-cmp", opts: backendOptions{unsupported: true}, code: 6, state: "failed"},
		{name: "other-repository-skipped", opts: backendOptions{unaffected: true}, state: "skipped"},
		{name: "confirmed-removal-policy", opts: backendOptions{remove: true}, code: 2, state: "changed", policy: true, pr: true},
		{name: "independent-instance-survives", code: 3, state: "changed", secondDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gitServer(t)
			a, _ := argoServer(t, "fictional-argo-0", tc.opts)
			endpoints := []string{a.URL}
			if tc.secondDenied {
				b, _ := argoServer(t, "fictional-argo-1", backendOptions{denied: true})
				endpoints = append(endpoints, b.URL)
			}
			cfg := configFile(t, g.URL, endpoints...)
			if tc.policy {
				appendPolicy(t, cfg)
			}
			args := []string{"diff", "--config", cfg, "--quiet", "--format", "json"}
			if tc.pr {
				args = append(args, "--pr", "https://github.com/example/charts/pull/12")
			} else {
				args = append(args, "--base", "main", "--head", "feature")
			}
			out, _, code := run(t, args...)
			var report domain.Report
			if code != tc.code || json.Unmarshal(out, &report) != nil || len(report.Applications) == 0 || string(report.Applications[0].State) != tc.state {
				t.Fatalf("installed contract mismatch: exit=%d expected=%d", code, tc.code)
			}
			if tc.pr && (report.Revisions.Base != ancestorSHA || report.Revisions.Head != headSHA) {
				t.Fatal("PR did not use immutable merge-base/head")
			}
			if tc.policy && (report.Policy == nil || report.Policy.Evaluation != "complete" || len(report.Policy.Violations) != 1) {
				t.Fatal("confirmed removal evidence missing")
			}
			if tc.secondDenied && (report.Status != "partial_failure" || len(report.Applications[0].Resources) != 1 || report.Applications[1].Execution != "denied") {
				t.Fatal("independent results were lost")
			}
		})
	}
	for _, scenario := range []string{"normal", "new", "modified", "removed", "denied"} {
		t.Run("recursive-"+scenario, func(t *testing.T) {
			g := gitServer(t)
			a, _ := recursiveServer(t, scenario, "fictional-argo-0", false, false)
			cfg := recursiveConfig(t, g.URL, a.URL)
			out, diagnostic, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--recursive", "--format", "json", "--quiet")
			var r domain.Report
			if json.Unmarshal(out, &r) != nil || r.Graph == nil || len(r.Applications) != 2 {
				t.Fatal("recursive process contract missing")
			}
			want := 1
			if scenario == "denied" {
				want = 3
			}
			if code != want {
				t.Fatalf("recursive exit=%d expected=%d", code, want)
			}
			if scenario == "new" || scenario == "modified" {
				found := false
				for _, app := range r.Applications {
					found = found || (app.ID.Name == "child" && app.Execution == "skipped" && app.SkipReason != "" && len(app.Resources) == 0)
				}
				if !found || r.Status != "success" || r.Graph.HeadComplete || !bytes.Contains(diagnostic, []byte("analysis_skipped")) {
					t.Fatal("basic analysis treated the skipped child as a failure, empty target or complete graph")
				}
			}
			if scenario == "removed" {
				found := false
				for _, app := range r.Applications {
					if app.ID.Name == "child" && app.Pruning != nil && app.Pruning.Certainty == "conditional" {
						found = true
					}
				}
				if !found {
					t.Fatal("conditional pruning evidence missing")
				}
			}
		})
	}
	t.Run("desired-spec-backend", func(t *testing.T) {
		cfg := mockConfig(t, true, true)
		out, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--recursive", "--format", "json", "--quiet")
		var r domain.Report
		if code != 1 || json.Unmarshal(out, &r) != nil || r.Graph == nil || !r.Graph.HeadComplete {
			t.Fatal("installed desired-spec render failed")
		}
	})
	t.Run("same-run-artifacts-and-repeat", func(t *testing.T) {
		g := gitServer(t)
		a, _ := argoServer(t, "fictional-argo-0", backendOptions{workload: true})
		cfg := configFile(t, g.URL, a.URL)
		dir := filepath.Join(t.TempDir(), "report directory with spaces")
		jsonPath := filepath.Join(dir, "result.json")
		mdPath := filepath.Join(dir, "result.md")
		var previous []byte
		for _, format := range []string{"pr-markdown", "markdown", "terminal"} {
			_, _, code := run(t, "diff", "--config", cfg, "--pr", "12", "--format", format, "--output", mdPath, "--json-output", jsonPath, "--quiet")
			data, err := os.ReadFile(jsonPath)
			if code != 1 || err != nil {
				t.Fatal("same-run artifacts failed")
			}
			if previous != nil && !bytes.Equal(previous, data) {
				t.Fatal("JSON changed across processes/formats")
			}
			previous = data
			md, err := os.ReadFile(mdPath)
			// The fixture changes the Service selector to payments and the
			// ConfigMap value to after only at head.
			if err != nil || !bytes.Contains(md, []byte("payments")) || !bytes.Contains(md, []byte("after")) {
				t.Fatal("Markdown lost the target diff")
			}
			if format == "pr-markdown" && (len(md) > 60*1024 || !bytes.Contains(md, []byte("<!-- kubeseer:preview -->"))) {
				t.Fatal("integration bound/marker failed")
			}
		}
		_, _, code := run(t, "diff", "--config", cfg, "--pr", "12", "--output", mdPath, "--json-output", mdPath)
		if code != 4 {
			t.Fatal("aliased artifacts accepted")
		}
	})
	t.Run("malformed-input-does-not-echo-secrets", func(t *testing.T) {
		_, diagnostic, code := run(t, "diff", "--unrecognized-secret-option=fictional-sensitive-error")
		if code != 4 {
			t.Fatal("argument rejection failed")
		}
		assertNoSecrets(t, "argument rejection", string(diagnostic), "fictional-sensitive-error")
	})
	t.Run("fork-rejected-before-render", func(t *testing.T) {
		g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/repos/example/charts/pulls/12" {
				t.Error("fork metadata caused an unexpected call")
				http.Error(w, "unexpected", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"base": map[string]any{"sha": baseSHA, "repo": map[string]any{"full_name": "example/charts"}},
				"head": map[string]any{"sha": headSHA, "repo": map[string]any{"full_name": "outside/charts"}},
			})
		}))
		defer g.Close()
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("fork rejection contacted Argo CD")
			http.Error(w, "unexpected", http.StatusBadRequest)
		}))
		defer a.Close()
		cfg := configFile(t, g.URL, a.URL)
		out, _, code := run(t, "diff", "--config", cfg, "--pr", "12", "--format", "json", "--quiet")
		var report domain.Report
		if code != 6 || json.Unmarshal(out, &report) != nil || report.Status != "failed" || len(report.Applications) != 0 || len(report.Issues) != 1 || report.Issues[0].Code != "untrusted_fork" {
			t.Fatal("unsupported fork lost its structured failure or was rendered")
		}
	})
	t.Run("output-failure-before-network", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("invalid output path contacted a provider")
			http.Error(w, "unexpected", http.StatusBadRequest)
		}))
		defer s.Close()
		cfg := configFile(t, s.URL, s.URL)
		_, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--output", t.TempDir())
		if code != 4 {
			t.Fatal("directory output path was accepted")
		}
	})
	t.Run("cross-instance-child", func(t *testing.T) {
		g := gitServer(t)
		parent, _ := recursiveServer(t, "normal", "fictional-argo-0", true, false)
		child, _ := recursiveServer(t, "normal", "fictional-argo-1", false, true)
		cfg := recursiveConfig(t, g.URL, parent.URL, child.URL)
		out, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--recursive", "--format", "json", "--quiet")
		var r domain.Report
		if code != 1 || json.Unmarshal(out, &r) != nil || r.Graph == nil || len(r.Graph.HeadEdges) != 1 || r.Graph.HeadEdges[0].Child.Instance != "instance-1" {
			t.Fatal("installed routing crossed an instance boundary")
		}
	})
	t.Run("application-limit", func(t *testing.T) {
		g := gitServer(t)
		a, _ := recursiveServer(t, "normal", "fictional-argo-0", false, false)
		cfg := recursiveConfig(t, g.URL, a.URL)
		appendProfile(t, cfg, "render:\n  maxApplications: 1\n")
		out, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--recursive", "--format", "json", "--quiet")
		var r domain.Report
		if code != 3 || json.Unmarshal(out, &r) != nil || r.Graph == nil || r.Graph.HeadComplete || !bytes.Contains(out, []byte("graph_limit_exceeded")) {
			t.Fatal("application limit advertised complete coverage")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		g := gitServer(t)
		a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
		cycle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/applications/api/manifests" {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fictional-argo-0" {
					t.Error("cycle fixture received mutation or wrong token")
				}
				app := testApp()
				app["apiVersion"], app["kind"] = "argoproj.io/v1alpha1", "Application"
				child, _ := json.Marshal(app)
				_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []string{string(child)}})
				return
			}
			a.Config.Handler.ServeHTTP(w, r)
		}))
		defer cycle.Close()
		cfg := recursiveConfig(t, g.URL, cycle.URL)
		out, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--recursive", "--format", "json", "--quiet")
		var report domain.Report
		if code != 3 || json.Unmarshal(out, &report) != nil || report.Graph == nil || report.Graph.HeadComplete || !bytes.Contains(out, []byte("graph_cycle")) {
			t.Fatal("cycle was accepted or hidden")
		}
	})
	t.Run("bounded-backend-timeout", func(t *testing.T) {
		g := gitServer(t)
		a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/manifests") {
				time.Sleep(1500 * time.Millisecond)
			}
			a.Config.Handler.ServeHTTP(w, r)
		}))
		defer slow.Close()
		cfg := configFile(t, g.URL, slow.URL)
		appendProfile(t, cfg, "render:\n  timeout: 1s\n")
		out, _, code := run(t, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json", "--quiet")
		var r domain.Report
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("backend timeout exit %d did not emit a JSON report", code)
		}
		if code != 6 || r.Status != "failed" || len(r.Applications) == 0 || len(r.Applications[0].Issues) == 0 {
			t.Fatalf("backend timeout: exit=%d status=%q applications=%d top-level issues=%d", code, r.Status, len(r.Applications), len(r.Issues))
		}
		for _, issue := range r.Applications[0].Issues {
			if issue.Code != "timeout" || issue.Stage == "" || issue.Repository == "" || issue.Revision == "" || issue.Application == nil {
				t.Fatal("backend timeout omitted actionable stage, revision or Application context")
			}
		}
	})
	t.Run("interrupt-preserves-cancelled-report", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("os.Process.Signal cannot send console interrupts on Windows")
		}
		g := gitServer(t)
		a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
		started := make(chan struct{}, 1)
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/manifests") {
				select {
				case started <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			a.Config.Handler.ServeHTTP(w, r)
		}))
		defer slow.Close()
		cfg := configFile(t, g.URL, slow.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json", "--quiet")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "PATH=")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal("could not start cancellation fixture")
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("process did not reach rendering within its bound")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal("could not send an interrupt")
		}
		var failure *exec.ExitError
		if !errors.As(cmd.Wait(), &failure) || ctx.Err() != nil || failure.ExitCode() != 7 {
			t.Fatal("interrupted process did not exit promptly with cancellation status")
		}
		var report domain.Report
		if json.Unmarshal(stdout.Bytes(), &report) != nil || report.Status != "cancelled" || !bytes.Contains(stdout.Bytes(), []byte(`"code": "cancelled"`)) {
			t.Fatal("interrupt lost the structured cancellation report")
		}
		assertNoSecrets(t, "cancelled process stdout", stdout.String(), "fictional-github-token", "fictional-argo-0")
		assertNoSecrets(t, "cancelled process stderr", stderr.String(), "fictional-github-token", "fictional-argo-0")
	})
}
