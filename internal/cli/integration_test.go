package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

func TestCLIEndToEnd(t *testing.T) {
	g := gitServer(t)
	a, revisions := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	r, code := runJSON(t, cfg, "--base", "main", "--head", "feature")
	if code != 1 || r.Status != "success" || !r.ChangesFound || len(r.Applications) != 1 {
		t.Fatalf("unexpected result: code=%d status=%s", code, r.Status)
	}
	if !renderedOnceEach(*revisions, baseSHA, headSHA) {
		t.Fatal("wrong render revisions")
	}
	app := r.Applications[0]
	if app.Inputs.ReleaseName != "api-test" || !app.Inputs.InlineValues || !app.Inputs.ValuesObject || strings.Join(app.Inputs.ValueFiles, ",") != "values.yaml,values-test.yaml" || len(app.Inputs.FileParameters) != 1 {
		t.Fatal("input provenance lost")
	}
	if len(app.Resources) != 1 || app.Resources[0].Changes[0].Path != "/spec/replicas" {
		t.Fatal("final resource diff missing")
	}
}

// TestReportCarriesComparisonFacts checks that every human and machine
// format states the facts the reporting contract requires, rather than
// fixing their exact text.
func TestReportCarriesComparisonFacts(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	report, code := runJSON(t, configFile(t, g.URL, a.URL), "--base", "main", "--head", "feature")
	if code != 1 || !report.ChangesFound || report.Revisions.Base != baseSHA || report.Revisions.Head != headSHA || len(report.Applications) == 0 {
		t.Fatalf("comparison did not complete: code=%d report=%+v", code, report)
	}
	app := report.Applications[0]
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		actual, err := reporter.Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range []string{baseSHA, headSHA, app.ID.Instance, app.ID.Name, string(report.Status)} {
			if !bytes.Contains(actual, []byte(fact)) {
				t.Fatalf("%s does not state %q", format, fact)
			}
		}
		for _, res := range app.Resources {
			if res.State != "unchanged" && !bytes.Contains(actual, []byte(res.ID.Name)) {
				t.Fatalf("%s omits changed resource %s", format, res.ID.Name)
			}
		}
		assertNoSecrets(t, format, string(actual), "fictional-")
	}
}

func TestDoctorDenialAndCancellation(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{denied: true})
	cfg := configFile(t, g.URL, a.URL)
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", cfg, "--format", "json"}, &out, &out); code != 5 {
		t.Fatalf("all-denied preflight returned %d", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if code := Run(ctx, []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json"}, &out, &out); code != 7 {
		t.Fatalf("cancelled preview returned %d", code)
	}
}

// doctor without --render states that it checked access and rendered nothing,
// on the console and in the report, and the reworded limitation names the
// render command; JSON keeps its Application state values.
func TestDoctorPreflightStatesNoComparison(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", cfg, "--format", "terminal"}, &out, &stderr); code != 0 {
		t.Fatalf("preflight returned %d: %s", code, stderr.String())
	}
	text := out.String() + stderr.String()
	for _, want := range []string{"Verdict: read access checked; targets were not rendered (exit 0)", "Read access checked; targets were not rendered.", "Run doctor --render --pr NUMBER or diff to verify rendering"} {
		if !strings.Contains(text, want) {
			t.Fatalf("preflight output lacks %q:\n%s", want, text)
		}
	}
	for _, absent := range []string{"No target changes found", "Verdict: no changes", "CMP", "reviewed fixture"} {
		if strings.Contains(text, absent) {
			t.Fatalf("preflight output states %q:\n%s", absent, text)
		}
	}
	out.Reset()
	if code := Run(context.Background(), []string{"doctor", "--config", cfg, "--format", "json"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), `"state": "unchanged"`) {
		t.Fatalf("preflight JSON state changed: code=%d\n%s", code, out.String())
	}
}

func TestCLIUsesMergeBaseForPR(t *testing.T) {
	g := gitServer(t)
	a, revisions := argoServer(t, "fictional-argo-0", backendOptions{})
	r, code := runJSON(t, configFile(t, g.URL, a.URL), "--pr", "12")
	if code != 1 || r.Revisions.Base != ancestorSHA || r.Revisions.Mode != "pr-merge-base-head" || !renderedOnceEach(*revisions, ancestorSHA, headSHA) {
		t.Fatal("PR comparison used base tip instead of merge-base")
	}
}

func TestCLIUsesPullRequestURL(t *testing.T) {
	g := gitServer(t)
	a, revisions := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	r, code := runJSON(t, cfg, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || r.Revisions.Base != ancestorSHA || r.Revisions.Head != headSHA || !renderedOnceEach(*revisions, ancestorSHA, headSHA) {
		t.Fatalf("PR URL did not preserve merge-base semantics: code=%d revisions=%+v", code, r.Revisions)
	}
	for _, input := range []string{"https://attacker.example/example/charts/pull/12", "https://github.com/example/other/pull/12"} {
		var out, stderr bytes.Buffer
		if got := Run(context.Background(), []string{"diff", "--config", cfg, "--pr", input}, &out, &stderr); got != 4 || out.Len() != 0 {
			t.Fatalf("untrusted PR input accepted: code=%d", got)
		}
	}
}

// TestCLIReportsUnreadablePRFileList keeps the cause of a refused PR file
// list in the report: one limitation names the code, stage, repository, PR
// and required access, and every format shows it. The list is evidence only,
// so the status, issues and exit code equal a run whose list was read.
func TestCLIReportsUnreadablePRFileList(t *testing.T) {
	refused := false
	g := newGitHubFake(t, githubFake{token: "fictional-github-token", routes: func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/repos/example/charts/pulls/12":
			fmt.Fprintf(w, `{"changed_files":1,"base":{"sha":%q,"repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, baseSHA, headSHA)
		case "/repos/example/charts/pulls/12/files":
			if refused {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
				return true
			}
			fmt.Fprint(w, `[{"filename":"charts/api/values.yaml"}]`)
		default:
			return false
		}
		return true
	}})
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	readable, readableCode := runJSON(t, cfg, "--pr", "12")
	refused = true
	report, code := runJSON(t, cfg, "--pr", "12")
	if code != readableCode || report.Status != readable.Status || len(report.Issues) != len(readable.Issues) {
		t.Fatalf("refused file list changed the outcome: code=%d status=%s issues=%d, readable code=%d status=%s issues=%d", code, report.Status, len(report.Issues), readableCode, readable.Status, len(readable.Issues))
	}
	refusal := func(r domain.Report) []string {
		var found []string
		for _, item := range r.Limitations {
			if strings.Contains(item, domain.CodeAccessDenied) {
				found = append(found, item)
			}
		}
		return found
	}
	if got := refusal(readable); len(got) != 0 {
		t.Fatalf("readable file list reported a refusal: %q", got)
	}
	got := refusal(report)
	if len(got) != 1 {
		t.Fatalf("refusal limitations = %q, want one", got)
	}
	for _, fact := range []string{"example/charts#12", "stage permission", "HTTP 403", "Pull requests: read"} {
		if !strings.Contains(got[0], fact) {
			t.Fatalf("refusal limitation %q lacks %q", got[0], fact)
		}
	}
	for _, format := range []string{"markdown", "terminal", "pr-markdown"} {
		var out, stderr bytes.Buffer
		if c := Run(context.Background(), []string{"diff", "--config", cfg, "--pr", "12", "--format", format, "--quiet"}, &out, &stderr); c != code {
			t.Fatalf("%s exit %d, want %d", format, c, code)
		}
		if !strings.Contains(out.String(), domain.CodeAccessDenied) || !strings.Contains(out.String(), "example/charts#12") {
			t.Fatalf("%s output does not show the refused file list:\n%s", format, out.String())
		}
	}
}

func TestCLIFailureBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name  string
		opts  backendOptions
		code  int
		issue string
		state string
	}{
		{"permission", backendOptions{denied: true}, 5, "access_denied", "denied"},
		{"context drift", backendOptions{drift: true}, 6, "render_context_changed", "failed"},
		{"head failure", backendOptions{headFailure: true}, 6, "backend_error", "failed"},
		{"masked secret stays unknown without failing", backendOptions{secret: true}, 1, "", "changed"},
		{"unsupported plugin", backendOptions{unsupported: true}, 6, "unsupported_source", "failed"},
		{"unaffected", backendOptions{unaffected: true}, 0, "", "skipped"},
		{"unchanged", backendOptions{unchanged: true}, 0, "", "unchanged"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gitServer(t)
			a, revisions := argoServer(t, "fictional-argo-0", tt.opts)
			r, code := runJSON(t, configFile(t, g.URL, a.URL), "--base", "main", "--head", "feature")
			if code != tt.code || string(r.Applications[0].State) != tt.state {
				t.Fatalf("got code=%d state=%s", code, r.Applications[0].State)
			}
			if tt.issue != "" && (len(r.Applications[0].Issues) == 0 || r.Applications[0].Issues[0].Code != tt.issue) {
				t.Fatal("actionable failure missing")
			}
			if tt.opts.secret {
				app := r.Applications[0]
				secretUnknown := false
				for _, res := range app.Resources {
					secretUnknown = secretUnknown || (res.ID.Kind == "Secret" && res.State == "unknown" && res.Comparison == "incomplete")
				}
				if app.Execution != "complete" || len(app.Issues) != 0 || !secretUnknown {
					t.Fatalf("masked Secret must stay unknown without failing the Application: %+v", app)
				}
			}
			if tt.opts.unaffected && len(*revisions) != 0 {
				t.Fatal("unaffected application rendered")
			}
		})
	}
}

func TestCLIPreservesIndependentInstance(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	b, _ := argoServer(t, "fictional-argo-1", backendOptions{denied: true})
	r, code := runJSON(t, configFile(t, g.URL, a.URL, b.URL), "--base", "main", "--head", "feature")
	if code != 3 || r.Status != "partial_failure" || !r.ChangesFound || len(r.Applications[0].Resources) != 1 || r.Applications[1].Execution != "denied" {
		t.Fatal("successful branch lost")
	}
}

func TestCLIInvalidArgumentsAndOutput(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	r, code := runJSON(t, cfg, "--pr", "12", "--base", "main")
	if code != 4 || r.Status != "invalid_configuration" {
		t.Fatal("ambiguous comparison accepted")
	}
	var out bytes.Buffer
	file := filepath.Join(t.TempDir(), "report.md")
	code = Run(context.Background(), []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--output", file}, &out, &out)
	if code != 1 {
		t.Fatal("file output failed")
	}
	data, err := os.ReadFile(file)
	if err != nil || !bytes.Contains(data, []byte("/spec/replicas")) {
		t.Fatal("Markdown output missing diff")
	}
}
