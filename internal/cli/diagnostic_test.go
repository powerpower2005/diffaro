package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestInitAccessDeniedHasInstanceRequestCredentialAndAction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		token  string
		cause  string
	}{
		{"missing", 401, "", "credential_missing"}, {"rejected", 401, "fictional-sensitive-token", "authentication_rejected"}, {"forbidden", 403, "fictional-sensitive-token", "request_forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DIFFARO_TEST_TOKEN", tc.token)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/applications" {
					t.Error("unexpected request")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("fictional-sensitive-response"))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "profile.yaml")
			var output, diagnostic bytes.Buffer
			code := Run(context.Background(), []string{"init", "--endpoint", server.URL, "--instance", "test", "--token-env", "DIFFARO_TEST_TOKEN", "--pr", "https://github.com/example/charts/pull/12", "--config", path}, &output, &diagnostic)
			if code != 5 || output.Len() != 0 {
				t.Fatal("denial exit behavior changed")
			}
			for _, text := range []string{"access denied (exit 5)", "Cause: " + tc.cause, "rerun the same command", "access_denied [stage=permission]", "Argo CD instance test Application inventory lookup", "GET /api/v1/applications", "HTTP", "DIFFARO_TEST_TOKEN", "Required action:"} {
				if !strings.Contains(diagnostic.String(), text) {
					t.Fatalf("missing %s: %s", text, diagnostic.String())
				}
			}
			if tc.status == 403 && !strings.Contains(diagnostic.String(), "applications/get") {
				t.Fatal("missing required inventory read access")
			}
			assertNoSecrets(t, "diagnostic with token or backend response", diagnostic.String(), "fictional-sensitive")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("denied init created a profile")
			}
		})
	}
}

func TestDeniedProjectDiagnosticsRemainVisibleWithFileOutputAndQuiet(t *testing.T) {
	g := gitServer(t)
	backend, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/demo" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte("fictional-sensitive-response"))
			return
		}
		backend.Config.Handler.ServeHTTP(w, r)
	}))
	defer denied.Close()
	good, _ := argoServer(t, "fictional-argo-1", backendOptions{})
	config := configFile(t, g.URL, denied.URL, good.URL)
	for _, format := range []string{"json", "markdown", "pr-markdown"} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report")
			var output, diagnostic bytes.Buffer
			code := Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", format, "--output", path, "--quiet"}, &output, &diagnostic)
			if code != 3 || output.Len() != 0 {
				t.Fatal("partial results or file output changed")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"Argo CD instance instance-0 AppProject lookup", "HTTP 403 Forbidden", "projects/get", "DIFFARO_TEST_ARGO_0"} {
				if !strings.Contains(diagnostic.String(), text) || !bytes.Contains(data, []byte(text)) {
					t.Fatalf("missing diagnostic %s", text)
				}
			}
			for _, text := range []string{"Credential environment: DIFFARO_TEST_ARGO_0", "system=argocd-rbac; instance=instance-0; resource=projects; action=get", "Access target: demo", "RBAC object: demo"} {
				if !strings.Contains(diagnostic.String(), text) {
					t.Fatalf("missing exact access guidance %q", text)
				}
			}
			assertNoSecrets(t, "diagnostic", diagnostic.String(), "fictional-")
			assertNoSecrets(t, "saved artifact with response body", string(data), "fictional-sensitive-response")
			if format == "json" {
				var report domain.Report
				if json.Unmarshal(data, &report) != nil || len(report.Applications) != 2 || report.Applications[1].Execution != "complete" {
					t.Fatal("independent successful branch was lost")
				}
				if report.Applications[0].Issues[0].ReasonCode != "request_forbidden" {
					t.Fatal("machine-readable access cause was lost")
				}
				access := report.Applications[0].Issues[0].RequiredAccess
				if access == nil || access.Resource != "projects" || access.Action != "get" || access.Object != "demo" || access.CredentialEnv != "DIFFARO_TEST_ARGO_0" || access.Instance != "instance-0" {
					t.Fatal("JSON lost the exact required permission")
				}
			}
			if !strings.Contains(diagnostic.String(), "partial failure (exit 3)") || strings.Contains(diagnostic.String(), "access denied (exit 5)") {
				t.Fatal("partial failure was described as total denial")
			}
		})
	}
}

func TestGitHubDenialIdentifiesPRReadAccess(t *testing.T) {
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer g.Close()
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := configFile(t, g.URL, a.URL)
	var output, diagnostic bytes.Buffer
	if Run(context.Background(), []string{"diff", "--config", config, "--pr", "12", "--format", "json", "--quiet"}, &output, &diagnostic) != 5 {
		t.Fatal("GitHub denial exit changed")
	}
	for _, text := range []string{"GitHub pull request lookup", "HTTP 403 Forbidden", "Pull requests: read", "DIFFARO_TEST_GITHUB", "organization approval/SSO"} {
		if !strings.Contains(diagnostic.String(), text) || !bytes.Contains(output.Bytes(), []byte(text)) {
			t.Fatalf("missing GitHub diagnostic %s", text)
		}
	}
	var report domain.Report
	if json.Unmarshal(output.Bytes(), &report) != nil || report.Status != "denied" {
		t.Fatal("stdout JSON contaminated")
	}
	assertNoSecrets(t, "diagnostic", diagnostic.String(), "fictional-github-token")
	if report.Issues[0].ReasonCode != "request_forbidden" || !strings.Contains(diagnostic.String(), "access denied (exit 5)") || !strings.Contains(diagnostic.String(), "Cause: request_forbidden") {
		t.Fatal("exit 5 did not explain the machine-readable cause")
	}
}

func TestUnclassifiedDenialStillExplainsNextAction(t *testing.T) {
	for _, withIssue := range []bool{false, true} {
		report := domain.NewReport()
		report.Status = "denied"
		if withIssue {
			report.Issues = append(report.Issues, domain.Issue{Code: "access_denied", Stage: "permission", Reason: "render binding denied"})
		}
		var diagnostic bytes.Buffer
		printReportDiagnostics(&diagnostic, report, security.Redactor{}, false)
		for _, text := range []string{"access denied (exit 5)", "Required action: check the token environment variable", "render access binding"} {
			if !strings.Contains(diagnostic.String(), text) {
				t.Fatalf("missing fallback guidance %q: %s", text, diagnostic.String())
			}
		}
		if strings.Contains(diagnostic.String(), "credential_missing") || strings.Contains(diagnostic.String(), "request_forbidden") {
			t.Fatal("unknown denial was assigned a guessed cause")
		}
	}
}

func TestDoctorDenialShowsCauseWithAndWithoutRender(t *testing.T) {
	for _, render := range []bool{false, true} {
		g := gitServer(t)
		denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		config := configFile(t, g.URL, denied.URL)
		t.Setenv("DIFFARO_TEST_ARGO_0", "")
		args := []string{"doctor", "--config", config, "--quiet", "--format", "json"}
		if render {
			args = append(args, "--render", "--base", "main", "--head", "feature")
		}
		var output, diagnostic bytes.Buffer
		code := Run(context.Background(), args, &output, &diagnostic)
		denied.Close()
		var report domain.Report
		if code != 5 || json.Unmarshal(output.Bytes(), &report) != nil || report.Status != "denied" {
			t.Fatalf("doctor render=%v lost the denial contract: exit=%d", render, code)
		}
		if !strings.Contains(diagnostic.String(), "access denied (exit 5)") || !strings.Contains(diagnostic.String(), "Cause: credential_missing") || !strings.Contains(diagnostic.String(), "set DIFFARO_TEST_ARGO_0") {
			t.Fatalf("doctor render=%v hid the next action: %s", render, diagnostic.String())
		}
		if len(report.Applications) != 1 || report.Applications[0].Issues[0].ReasonCode != "credential_missing" {
			t.Fatal("doctor JSON lost the access cause")
		}
	}
}

func TestIssueAtBaseAndHeadPrintsOnce(t *testing.T) {
	report := domain.NewReport()
	report.Status = "partial_failure"
	app := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	for _, side := range []struct{ name, revision string }{{"base", strings.Repeat("a", 40)}, {"head", strings.Repeat("b", 40)}} {
		report.Issues = append(report.Issues, domain.Issue{Code: "not_found", Stage: "render", Reason: "source path is missing", Action: "check the path", Application: &app, Side: side.name, Revision: side.revision})
	}
	var diagnostic bytes.Buffer
	printReportDiagnostics(&diagnostic, report, security.Redactor{}, false)
	out := diagnostic.String()
	if strings.Count(out, "not_found [stage=render]") != 1 || !strings.Contains(out, "Side: base, head") || !strings.Contains(out, "Revision: "+strings.Repeat("a", 40)+", "+strings.Repeat("b", 40)) {
		t.Fatalf("base and head failure not merged:\n%s", out)
	}
}

// TestIssueConsoleOutputIsBoundedDeduplicatedAndRedacted checks each step of
// console diagnostics: identical issues collapse, distinct causes print up
// to the record limit, the remainder is counted and secrets are masked.
func TestIssueConsoleOutputIsBoundedDeduplicatedAndRedacted(t *testing.T) {
	const limit = 5 // console records without --verbose
	issue := domain.Issue{Code: "access_denied", Stage: "permission", Reason: strings.Repeat("fictional-secret", 200), Action: "Authorization: Bearer fictional-header"}
	report := domain.NewReport()
	report.Status = "denied"
	distinct := 0
	for i := 0; i < limit+2; i++ {
		cause := issue
		cause.Repository = "https://example.invalid/repository-" + string(rune('a'+i))
		distinct++
		// Every cause appears three times; duplicates must collapse.
		report.Issues = append(report.Issues, cause, cause, cause)
	}
	var diagnostic bytes.Buffer
	printReportDiagnostics(&diagnostic, report, security.Redactor{Secrets: []string{"fictional-secret"}}, false)
	out := diagnostic.String()
	if got := strings.Count(out, "access_denied [stage=permission]"); got != limit {
		t.Fatalf("printed %d records, want the limit %d", got, limit)
	}
	if !strings.Contains(out, fmt.Sprintf("%d more issue(s)", distinct-limit)) {
		t.Fatalf("remaining distinct causes not counted:\n%s", out)
	}
	assertNoSecrets(t, "console diagnostics", out, "fictional-")
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 1250 {
			t.Fatal("unbounded diagnostic line")
		}
	}
}

func TestIssueConsoleCannotInjectExtraLines(t *testing.T) {
	issue := domain.Issue{Code: "access_denied", Stage: "permission", Reason: "request failed\nforged diagnostic", Action: "retry\nforged action", Application: &domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api\nforged application"}}
	var diagnostic bytes.Buffer
	printIssue(&diagnostic, issue, security.Redactor{})
	if strings.Count(diagnostic.String(), "\n") != 3 || strings.Contains(diagnostic.String(), "\nforged") {
		t.Fatal("untrusted identifiers injected console lines")
	}
}

func TestRequiredAccessConsoleIsRedactedBoundedAndDistinguishesTargets(t *testing.T) {
	report := domain.NewReport()
	report.Status = "denied"
	for _, target := range []string{"demo", "other"} {
		report.Issues = append(report.Issues, domain.Issue{Code: "access_denied", Stage: "permission", Reason: "HTTP 403", RequiredAccess: &domain.AccessRequirement{System: "argocd-rbac", Resource: "projects", Action: "get", Target: target, Object: target, CredentialEnv: "DIFFARO_ARGOCD_TOKEN", Scope: strings.Repeat("fictional-private-value", 150) + "\nforged scope"}})
	}
	var output bytes.Buffer
	printReportDiagnostics(&output, report, security.Redactor{Secrets: []string{"fictional-private-value"}}, false)
	if strings.Count(output.String(), "Required access:") != 2 || strings.Contains(output.String(), "\nforged") {
		t.Fatal("permissions were conflated or injected")
	}
	assertNoSecrets(t, "permission diagnostics", output.String(), "fictional-private-value")
	for _, line := range strings.Split(output.String(), "\n") {
		if len([]rune(line)) > 1250 {
			t.Fatal("unbounded access metadata")
		}
	}
}

func TestMissingRoutingDiagnosticsAndExplicitRecovery(t *testing.T) {
	g := gitServer(t)
	backend, renders := recursiveServer(t, "normal", "fictional-argo-0", false, false)
	config := configFile(t, g.URL, backend.URL)
	for _, format := range []string{"json", "markdown", "pr-markdown"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path, jsonPath := filepath.Join(dir, "preview"), filepath.Join(dir, "preview.json")
			var output, diagnostic bytes.Buffer
			args := []string{"diff", "--config", config, "--recursive", "--base", "main", "--head", "feature", "--format", format, "--output", path, "--quiet"}
			if format == "json" {
				jsonPath = path
			} else {
				args = append(args, "--json-output", jsonPath)
			}
			code := Run(context.Background(), args, &output, &diagnostic)
			if code != 3 || output.Len() != 0 || renders.Load() != 0 {
				t.Fatalf("missing routing guessed a child instance or changed partial exit behavior: exit=%d; %s", code, diagnostic.String())
			}
			data, err := os.ReadFile(jsonPath)
			var report domain.Report
			if err != nil || json.Unmarshal(data, &report) != nil || report.Status != "partial_failure" || len(report.Applications) != 1 || len(report.Applications[0].Resources) == 0 || report.Graph == nil || report.Graph.BaseComplete || report.Graph.HeadComplete || len(report.Issues) != 2 {
				t.Fatal("routing failure lost parent targets or incomplete coverage")
			}
			artifact, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"instance-0/argocd/api", "test-cluster", "argocd/child", "child project=", "routing.clusters", "parentInstance", "routing.defaultInstance", "only if all descendants"} {
				if !strings.Contains(diagnostic.String(), text) || !bytes.Contains(artifact, []byte(text)) {
					t.Fatalf("console or %s lost routing guidance %q", format, text)
				}
			}
			if report.Issues[0].Side != "base" || report.Issues[1].Side != "head" {
				t.Fatal("routing failure lost revision context")
			}
			assertNoSecrets(t, "routing failure artifact", string(data), "fictional-")
		})
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, routing := range []string{"routing:\n  defaultInstance: instance-0\n", "routing:\n  clusters:\n    - cluster: test-cluster\n      namespace: argocd\n      instance: instance-0\n", "routing:\n  clusters:\n    - parentInstance: instance-0\n      cluster: test-cluster\n      namespace: argocd\n      instance: instance-0\n"} {
		path := filepath.Join(t.TempDir(), "repaired.yaml")
		if err := os.WriteFile(path, append(append([]byte{}, data...), []byte(routing)...), 0600); err != nil {
			t.Fatal(err)
		}
		report, code := runJSON(t, path, "--recursive", "--base", "main", "--head", "feature")
		if code != 1 || report.Status != "success" || len(report.Applications) != 2 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
			t.Fatal("explicit routing guidance did not recover the full comparison")
		}
	}
}

func TestIssueConsoleKeepsRepositoryAndStageDifferences(t *testing.T) {
	issue := domain.Issue{Code: "render_failed", Stage: "render", Reason: "values unavailable", Action: "check values", Repository: "example/charts", Revision: strings.Repeat("a", 40)}
	otherRepository, otherStage := issue, issue
	otherRepository.Repository = "example/values"
	otherStage.Stage = "discovery"
	report := domain.NewReport()
	report.Status = "partial_failure"
	report.Issues = []domain.Issue{issue, otherRepository, otherStage, issue}
	var output bytes.Buffer
	printReportDiagnostics(&output, report, security.Redactor{}, false)
	if strings.Count(output.String(), "render_failed [stage=render]") != 2 || strings.Count(output.String(), "render_failed [stage=discovery]") != 1 {
		t.Fatalf("console collapsed distinct issues or kept duplicates\n%s", output.String())
	}
}

func TestConsoleStatesExcludedPreexistingFailures(t *testing.T) {
	report := domain.NewReport()
	report.Status, report.ChangesFound, report.PreexistingFailuresExcluded = "success", true, 1
	app := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "old"}
	report.Applications = []domain.ApplicationResult{{ID: app, Execution: "failed", FailureOrigin: "preexisting"}}
	report.Issues = []domain.Issue{{Code: "local_render_failed", Stage: "render", Reason: "chart path is missing", Action: "check the path", Application: &app, Side: "base"}}
	var diagnostic bytes.Buffer
	printReportDiagnostics(&diagnostic, report, security.Redactor{}, false)
	out := diagnostic.String()
	if !strings.Contains(out, "Diffaro Verdict: changes found (exit 1)") || !strings.Contains(out, "Not counted: 1 Application(s) already failing before this PR (failures.excludePreexistingFromExit)") || !strings.Contains(out, "chart path is missing") || !strings.Contains(out, "Failure origin: already failing before this PR") {
		t.Fatalf("console hides the excluded failure:\n%s", out)
	}
}

// A run-level failure names its instance instead of an empty Application,
// a credential without an environment name says where the token came from,
// and each further check of an action has its own line.
func TestIssueLinesNameInstanceCredentialAndSteps(t *testing.T) {
	issue := domain.Issue{Code: "access_denied", Stage: "permission", Reason: "HTTP 401 Unauthorized", Action: "verify the profile token; check expiration; check the gateway",
		Application: &domain.ApplicationID{Instance: "east"}, RequiredAccess: &domain.AccessRequirement{System: "argocd-rbac", Instance: "east", Resource: "applications", Action: "get"}}
	var out bytes.Buffer
	printIssue(&out, issue, security.Redactor{})
	text := out.String()
	for _, want := range []string{"  Argo CD instance: east\n", "  Credential: token from the profile or a credential flag\n", "  Required action: verify the profile token\n    - check expiration\n    - check the gateway\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "east//") || strings.Contains(text, "environment name unavailable") {
		t.Fatalf("empty Application or internal wording shown:\n%s", text)
	}
}
