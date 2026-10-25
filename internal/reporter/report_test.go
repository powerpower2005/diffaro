package reporter

import (
	"diffaro/internal/domain"
	"diffaro/internal/security"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAccessCauseIsVisibleInBothReportsAndOptionalInJSON(t *testing.T) {
	for _, cause := range []string{"credential_missing", "authentication_rejected", "request_forbidden", ""} {
		r := domain.NewReport()
		r.Status = "denied"
		r.Issues = []domain.Issue{{Code: "access_denied", ReasonCode: cause, Stage: "permission", Reason: "HTTP request was rejected", Action: "check read access"}}
		for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
			data, err := Encode(r, format, security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			if cause != "" && !strings.Contains(string(data), cause) {
				t.Fatalf("%s lost %s", format, cause)
			}
			if format == "json" {
				var got domain.Report
				if json.Unmarshal(data, &got) != nil || got.SchemaVersion != "v1alpha1" || got.ExitCode() != 5 || got.Issues[0].Code != "access_denied" || got.Issues[0].ReasonCode != cause {
					t.Fatal("aggregate denial contract changed")
				}
				if cause == "" && strings.Contains(string(data), "reasonCode") {
					t.Fatal("optional cause must be absent when unclassified")
				}
			}
		}
	}
}

func TestRequiredAccessSurvivesReportsAndRedaction(t *testing.T) {
	r := domain.NewReport()
	r.Status = "denied"
	r.Issues = []domain.Issue{{Code: "access_denied", Stage: "permission", Reason: "HTTP 403 Forbidden", Action: "check access", RequiredAccess: &domain.AccessRequirement{System: "argocd-rbac", Instance: "test", CredentialEnv: "DIFFARO_ARGOCD_TOKEN", Resource: "projects", Action: "get", Target: "demo", Object: "demo", Scope: "fictional-private-value"}}}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{Secrets: []string{"fictional-private-value"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"DIFFARO_ARGOCD_TOKEN", "projects", "get", "demo", "argocd-rbac"} {
			if !strings.Contains(string(data), value) {
				t.Fatalf("%s lost %s", format, value)
			}
		}
		if strings.Contains(string(data), "fictional-private-value") {
			t.Fatal("metadata secret leaked")
		}
		if format == "json" {
			var got domain.Report
			if json.Unmarshal(data, &got) != nil || got.ExitCode() != 5 || got.SchemaVersion != "v1alpha1" || got.Issues[0].RequiredAccess.CredentialEnv != "DIFFARO_ARGOCD_TOKEN" || got.Issues[0].RequiredAccess.Object != "demo" {
				t.Fatal("structured requirement was lost or masked as a credential value")
			}
		}
	}
}

func TestPRMarkdownKeepsFailuresAndDiffVisible(t *testing.T) {
	r := domain.NewReport()
	r.Status, r.ChangesFound = "partial_failure", true
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	issue := domain.Issue{Code: "render_failed", Reason: "fictional-secret was unavailable", Action: "check trusted backend", Application: &id}
	r.Issues = []domain.Issue{issue}
	r.Applications = []domain.ApplicationResult{{ID: id, State: "changed", Execution: "complete", Resources: []domain.ResourceDiff{{
		ID: domain.ResourceID{Kind: "Deployment", Namespace: "demo", Name: "api"}, State: "changed", Comparison: "complete", Changes: []domain.Change{{Path: "/spec/replicas", Operation: "replace", Before: json.Number("2"), After: json.Number("3")}},
	}}}, {ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "worker"}, State: "added", Execution: "failed", Issues: []domain.Issue{issue}}}
	result, err := Encode(r, "pr-markdown", security.Redactor{Secrets: []string{"fictional-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	output := string(result)
	for _, want := range []string{prMarker, "partial_failure", "## Needs attention: 1 Application not compared", "Redaction: `enabled`", "### `api` · changed", "### `worker` · added", "**Not compared (failed)**", "test/argocd/api", "/spec/replicas", "&#91;REDACTED&#93;", strings.Repeat("b", 40)} {
		if !strings.Contains(output, want) {
			t.Fatalf("PR result missing %q", want)
		}
	}
	if strings.Contains(output, "fictional-secret") || strings.Count(output, "(`render_failed`)") != 1 {
		t.Fatal("secret leaked or duplicate issue confused the summary")
	}
}

func TestPRMarkdownIsBoundedAndValidUTF8(t *testing.T) {
	r := domain.NewReport()
	r.Revisions.Head = strings.Repeat("b", 40)
	for i := 0; i < 400; i++ {
		r.RenderBackends = append(r.RenderBackends, domain.BackendInfo{Instance: strings.Repeat("가", 90), Kind: "argocd-repo-server", ContextID: "fixture"})
	}
	result, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) > MaxPRMarkdownBytes || !utf8.Valid(result) || !strings.HasPrefix(string(result), prMarker) || !strings.Contains(string(result), "Comment shortened to 60 KiB") {
		t.Fatal("PR artifact overflowed or lost its truncation notice")
	}
	if strings.Count(string(result), "<details>") != strings.Count(string(result), "</details>") {
		t.Fatal("truncation left evidence folded over the truncation notice")
	}
	if !strings.Contains(string(result), r.Revisions.Head) {
		t.Fatal("truncation lost the resolved head SHA")
	}
}

func TestPRLevelsKeepUncertaintyVisibleAndSharedEdgesDiscoverable(t *testing.T) {
	r := domain.NewReport()
	r.Status = "partial_failure"
	child := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "shared"}
	r.Graph = &domain.GraphSummary{Changes: []domain.GraphEdgeChange{
		{Parent: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "root"}, Child: child, State: "unchanged"},
		{Parent: domain.ApplicationID{Instance: "two", Namespace: "argocd", Name: "root"}, Child: child, State: "unchanged"},
	}}
	result, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	output := string(result)
	summary := strings.Split(output, "<details>")[0]
	for _, want := range []string{"Comparison is incomplete", "Graph discovery is incomplete"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary hides %q", want)
		}
	}
	for _, want := range []string{"one/argocd/root", "two/argocd/root", "test/argocd/shared"} {
		if !strings.Contains(output, want) {
			t.Fatalf("folded report loses %q", want)
		}
	}
}

func TestEscapesMarkdownAndCredentials(t *testing.T) {
	r := domain.NewReport()
	r.Revisions.Repository = "<script>[x](javascript:bad) `fictional-known-token`"
	r.Issues = append(r.Issues, domain.Issue{Code: "failure", Reason: "Authorization: Bearer fictional-header", Action: "retry"})
	b, err := Encode(r, "markdown", security.Redactor{Secrets: []string{"fictional-known-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "<script>") || strings.Contains(string(b), "fictional-") || strings.Contains(string(b), "[x](") {
		t.Fatal("unsafe Markdown or credential escaped filtering")
	}
}

func TestJSONNumbersAndNulls(t *testing.T) {
	r := domain.NewReport()
	r.Applications = []domain.ApplicationResult{{Resources: []domain.ResourceDiff{{Changes: []domain.Change{{Path: "/spec/count", Operation: "replace", Before: nil, After: json.Number("9007199254740993")}}}}}}
	b, err := Encode(r, "json", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "9007199254740993") || !strings.Contains(string(b), `"before": null`) {
		t.Fatal("numeric precision or null before-state lost")
	}
	markdown, err := Encode(r, "markdown", security.Redactor{})
	if err != nil || !strings.Contains(string(markdown), "9007199254740993") {
		t.Fatal("Markdown lost numeric precision")
	}
}

func TestShortKeepsUTF8AndByteBound(t *testing.T) {
	// 3999 ASCII bytes followed by 3-byte runes: a byte cut at 4000 would
	// split the first rune.
	value := strings.Repeat("a", 3999) + strings.Repeat("값", 10)
	got := short([]byte(value))
	if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("shortened value split a rune")
	}
	if !strings.HasSuffix(got, esc("… [truncated; use JSON]")) || strings.Contains(got, "값") {
		t.Fatalf("expected truncation at the last whole rune within 4000 bytes, got suffix %q", got[len(got)-40:])
	}
	if exact := strings.Repeat("값", 1333); short([]byte(exact)) != esc(exact) {
		t.Fatal("value within 4000 bytes must not be truncated")
	}
}
