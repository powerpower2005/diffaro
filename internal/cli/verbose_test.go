package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestBasicAPIDiagnosticsForSkippedAndDeniedChildren(t *testing.T) {
	for _, scenario := range []string{"new", "modified", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			git := gitServer(t)
			backend, _ := recursiveServer(t, scenario, "fictional-argo-0", false, false)
			config := recursiveConfig(t, git.URL, backend.URL)
			args := []string{"diff", "--config", config, "--recursive", "--base", "main", "--head", "feature", "--format", "json", "--quiet"}
			var output, diagnostic bytes.Buffer
			code := Run(context.Background(), args, &output, &diagnostic)
			wantCode := 1
			if scenario == "denied" {
				wantCode = 3
			}
			if code != wantCode || diagnostic.Len() == 0 {
				t.Fatalf("default output hid the analysis boundary: code=%d %s", code, diagnostic.String())
			}
			original := append([]byte(nil), output.Bytes()...)
			output.Reset()
			diagnostic.Reset()
			if Run(context.Background(), append(args, "--verbose"), &output, &diagnostic) != wantCode || !bytes.Equal(original, output.Bytes()) {
				t.Fatal("verbose changed comparison JSON or exit behavior")
			}
			var report domain.Report
			if json.Unmarshal(output.Bytes(), &report) != nil {
				t.Fatal("verbose contaminated machine output")
			}
			for _, required := range []string{"Base: " + baseSHA, "Head: " + headSHA, "Argo CD instance: instance-0", "Graph path: instance-0/argocd/api -> instance-0/argocd/child", "Destination cluster: test-cluster", "Render backend: argocd-application-api", "Required action:"} {
				if !strings.Contains(diagnostic.String(), required) {
					t.Fatalf("missing detailed context %q: %s", required, diagnostic.String())
				}
			}
			switch scenario {
			case "new":
				if !strings.Contains(diagnostic.String(), "not_found (HTTP 404)") || !strings.Contains(diagnostic.String(), "private Application may be hidden") || !strings.Contains(diagnostic.String(), "Side: head") {
					t.Fatal("new child cause or uncertainty is missing")
				}
			case "modified":
				if !strings.Contains(diagnostic.String(), "/source/helm/releaseName") || strings.Contains(diagnostic.String(), "changed-release") || !strings.Contains(diagnostic.String(), "Side: head") {
					t.Fatal("changed input path missing or actual value leaked")
				}
			case "denied":
				for _, required := range []string{"HTTP 403 Forbidden", "Cause: request_forbidden", "resource=applications; action=get", "Credential environment: DIFFARO_TEST_ARGO_0"} {
					if !strings.Contains(diagnostic.String(), required) {
						t.Fatal("access cause missing:", required)
					}
				}
				if strings.Contains(diagnostic.String(), "analysis_skipped") {
					t.Fatal("permission denial became a skip")
				}
			}
			assertNoSecrets(t, "verbose diagnostic with credentials or raw backend text", diagnostic.String(), "fictional-")
		})
	}
}

func TestVerboseDoctorAndFlagValidation(t *testing.T) {
	git := gitServer(t)
	backend, _ := recursiveServer(t, "new", "fictional-argo-0", false, false)
	var output, diagnostic bytes.Buffer
	if Run(context.Background(), []string{"doctor", "--config", recursiveConfig(t, git.URL, backend.URL), "--render", "--recursive", "--base", "main", "--head", "feature", "--format", "json", "--quiet", "--verbose"}, &output, &diagnostic) != 1 || !strings.Contains(diagnostic.String(), "Graph path:") || !strings.Contains(diagnostic.String(), "not_found (HTTP 404)") {
		t.Fatal("doctor did not expose detailed skip context")
	}
	output.Reset()
	diagnostic.Reset()
	if Run(context.Background(), []string{"validate", "--verbose", "--config", "absent.yaml"}, &output, &diagnostic) != 4 || !strings.Contains(diagnostic.String(), "only by diff and doctor") {
		t.Fatal("verbose validation contract changed")
	}
}

func TestSkipDiagnosticsKeepRevisionCausesAndOppositeGraphUncertainty(t *testing.T) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "child"}
	report := domain.NewReport()
	report.Applications = []domain.ApplicationResult{{ID: id, Execution: "skipped", SkipReason: "overall boundary",
		BaseTarget: &domain.TargetSummary{Execution: "skipped", Repository: "https://example.com/base.git", Revision: baseSHA, SkipReason: "base lookup unavailable"},
		HeadTarget: &domain.TargetSummary{Execution: "skipped", Repository: "https://example.com/head.git", Revision: headSHA, SkipReason: "head input mismatch"},
	}}
	var output bytes.Buffer
	printReportDiagnostics(&output, report, security.Redactor{}, true)
	for _, required := range []string{"base lookup unavailable", "head input mismatch", "Side: base", "Side: head", "Revision: " + baseSHA, "Revision: " + headSHA, "https://example.com/base.git", "https://example.com/head.git"} {
		if !strings.Contains(output.String(), required) {
			t.Fatal("revision-specific skip cause missing:", required)
		}
	}
	output.Reset()
	report.Applications[0].BaseTarget.Execution = "complete"
	report.Applications[0].HeadTarget = nil
	report.Applications[0].SkipReason = "Application presence is unknown because the opposite graph is incomplete"
	printReportDiagnostics(&output, report, security.Redactor{}, true)
	if !strings.Contains(output.String(), "opposite graph is incomplete") || strings.Contains(output.String(), "Side:") || strings.Contains(output.String(), "Revision:") {
		t.Fatal("opposite-graph uncertainty was attributed to a successful target")
	}
	output.Reset()
	report.Applications = nil
	for i := 0; i < 205; i++ {
		report.Applications = append(report.Applications, domain.ApplicationResult{ID: id, Execution: "skipped", SkipReason: "analysis boundary"})
	}
	printReportDiagnostics(&output, report, security.Redactor{}, true)
	if strings.Count(output.String(), "analysis_skipped [stage=render/discovery]") != 200 || !strings.Contains(output.String(), "5 more skipped target(s)") {
		t.Fatal("skip diagnostics exceeded the record budget")
	}
}

func TestVerboseDiagnosticsAreBoundedRedactedAndRetainIssueScope(t *testing.T) {
	report := domain.NewReport()
	report.Status = "partial_failure"
	for i := 0; i < 205; i++ {
		id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: fmt.Sprintf("app-%03d", i)}
		issue := domain.Issue{Code: "access_denied", Stage: "render", Application: &id, Reason: "fictional-private-value\nforged reason", Action: "Authorization: Bearer fictional-header", Revision: baseSHA, Side: "base", SourcePosition: 2}
		report.Applications = append(report.Applications, domain.ApplicationResult{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, Cluster: "fictional-private-value\nforged cluster", Issues: []domain.Issue{issue}})
	}
	var output bytes.Buffer
	printReportDiagnostics(&output, report, security.Redactor{Secrets: []string{"fictional-private-value"}}, true)
	if strings.Count(output.String(), "access_denied [stage=render]") != 200 || !strings.Contains(output.String(), "5 more issue(s)") || !strings.Contains(output.String(), "Source position: 2") || strings.Contains(output.String(), "\nforged") {
		t.Fatal("verbose diagnostic limits or scope failed")
	}
	assertNoSecrets(t, "verbose diagnostic", output.String(), "fictional-")
	for _, line := range strings.Split(output.String(), "\n") {
		if len([]rune(line)) > 1250 {
			t.Fatal("unbounded diagnostic line")
		}
	}
	output.Reset()
	bounded := &diagnosticOutput{writer: &output, remaining: 8}
	_, _ = bounded.Write([]byte("small\n"))
	_, _ = bounded.Write([]byte("fictional-secret-that-must-not-be-partially-written"))
	bounded.finish()
	if strings.Contains(output.String(), "fictional") || !strings.Contains(output.String(), "Diagnostic byte limit reached") {
		t.Fatal("byte bound did not preserve whole diagnostic writes")
	}
}
