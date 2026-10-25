package reporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// The fixture is a local Helm report of a new child Application, saved from
// TestExternalHelmFirstUseAndNewChild.
func TestTerminalNewChild(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "new-child-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report domain.Report
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	got, err := Encode(report, "terminal", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[+] App demo/argocd/new-api", "Base target: absent (new Application)", "Deployment/new-api", "Service/new-api", "/spec/replicas: 3", "image=\"example.invalid/api:1\"", "/spec/ports:", "Exit code: 1", report.Revisions.Head, "local-context-unverified", "parent-target-manifest"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q", want)
		}
	}
	if bytes.Contains(got, []byte("```")) || bytes.Contains(got, []byte("## ")) || bytes.Contains(got, []byte("\x1b")) {
		t.Fatal("terminal contains markup or ANSI")
	}
}

func TestTerminalPartialResultsRedactionAndControls(t *testing.T) {
	r := domain.NewReport()
	r.Status, r.ChangesFound = "partial_failure", true
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Mode: "target-to-target"}
	id := domain.ApplicationID{Instance: "west", Namespace: "argocd", Name: "api"}
	issue := domain.Issue{Application: &id, Code: "access_denied", ReasonCode: "request_forbidden", Stage: "render", Repository: "example/charts", Revision: r.Revisions.Head, Reason: "fictional-sensitive-value\n[=] forged success\x1b[2J\u202e", Action: "check read access", RequiredAccess: &domain.AccessRequirement{System: "argocd-rbac", CredentialEnv: "TEST_ARGO_TOKEN", Resource: "projects", Action: "get", Target: "demo"}}
	r.Issues = []domain.Issue{issue}
	r.Graph = &domain.GraphSummary{BaseComplete: true, HeadComplete: false}
	r.Applications = []domain.ApplicationResult{
		{ID: id, State: "unknown", Execution: "denied", Issues: []domain.Issue{issue}},
		{ID: domain.ApplicationID{Instance: "east", Name: "new-child"}, State: "added", Execution: "skipped", SkipReason: "cluster capabilities unavailable"},
		{ID: domain.ApplicationID{Instance: "east", Name: "stable"}, State: "unchanged", Execution: "complete", Resources: []domain.ResourceDiff{{State: "unchanged", Comparison: "complete"}}},
		{ID: domain.ApplicationID{Instance: "east", Name: "changed-target"}, State: "unchanged", Execution: "complete", Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Kind: "Deployment", Name: "api"}, State: "changed", Comparison: "incomplete", Changes: []domain.Change{{Path: "/spec/replicas", Operation: "replace", Before: 2, After: 3}, {Path: "/spec/token", Operation: "replace", Before: "fictional-sensitive-value", After: security.Mask, Redacted: true}}}}},
	}
	got, err := Encode(r, "terminal", security.Redactor{Secrets: []string{"fictional-sensitive-value"}})
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, want := range []string{"Exit code: 3", "1 failed/denied apps, 1 skipped apps, 1 incomplete", "Analysis is incomplete", "Graph discovery is incomplete", "Skipped: cluster capabilities unavailable", "request_forbidden", "Required access: argocd-rbac", "TEST_ARGO_TOKEN", "Resource: projects", "Action: check read access", "1 unchanged apps omitted", "changed-target", "/spec/replicas: 2 -> 3", "/spec/token: value changed (hidden)", "\\u000a", "\\u202e", "Issues: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "fictional-sensitive-value") || strings.Contains(out, "\n[=] forged") || strings.Contains(out, "\x1b") || strings.Contains(out, "\u202e") || strings.Count(out, "[!] access_denied") != 1 {
		t.Fatal("unsafe or duplicate issue output")
	}
	// Presentation must not mutate the comparison or depend on app/resource order.
	r.Applications[0], r.Applications[3] = r.Applications[3], r.Applications[0]
	repeat, _ := Encode(r, "terminal", security.Redactor{Secrets: []string{"fictional-sensitive-value"}})
	if !bytes.Equal(got, repeat) {
		t.Fatal("app ordering changed the report")
	}
}

func TestTerminalBoundsKeepSummaryAndHead(t *testing.T) {
	r := domain.NewReport()
	r.Status = "partial_failure"
	r.Revisions.Head = strings.Repeat("b", 40)
	for i := 0; i < 400; i++ {
		r.Applications = append(r.Applications, domain.ApplicationResult{ID: domain.ApplicationID{Instance: "test", Name: fmt.Sprintf("%04d", i)}, State: "added", Execution: "skipped", SkipReason: strings.Repeat("가", 400)})
	}
	got, err := Encode(r, "terminal", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxTerminalBytes || !utf8.Valid(got) || !strings.Contains(string(got), "Output shortened to 64 KiB") || !strings.Contains(string(got), r.Revisions.Head) || !strings.Contains(string(got), "400 skipped apps") || !strings.Contains(string(got), "Analysis is incomplete") || !strings.HasSuffix(string(got), "full report.\n") {
		t.Fatal("bounded output lost its summary, valid encoding or truncation notice")
	}
}

func TestTerminalMissingVersusNullAndEmptyValues(t *testing.T) {
	w := &terminalWriter{}
	terminalChange(w, "", domain.Change{Path: "/new", Operation: "add", After: nil})
	terminalChange(w, "", domain.Change{Path: "/old", Operation: "remove", Before: ""})
	terminalChange(w, "", domain.Change{Path: "/value", Operation: "replace", Before: nil, After: ""})
	for _, want := range []string{"/new: <absent> -> null", "/old: \"\" -> <absent>", "/value: null -> \"\""} {
		if !strings.Contains(w.b.String(), want) {
			t.Fatal("lost value semantics", w.b.String())
		}
	}
}

func TestTerminalUnchangedScopeStaysVisible(t *testing.T) {
	r := domain.NewReport()
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	r.Applications = []domain.ApplicationResult{{ID: id, Root: id, State: "unchanged", Execution: "complete"}}
	got, err := Encode(r, "terminal", security.Redactor{})
	if err != nil || !strings.Contains(string(got), "Starting roots/apps (1): east/argocd/api") || !strings.Contains(string(got), "1 unchanged apps omitted") {
		t.Fatal("unchanged comparison lost the analyzed root", err, string(got))
	}
}

// The terminal report counts standing boundaries instead of repeating them
// and keeps this run's limitations in full; Markdown and JSON keep every
// limitation.
func TestTerminalCountsStandingLimitations(t *testing.T) {
	report := domain.NewReport()
	runSpecific := "Argo CD instance ea version could not be read (timeout); the comparison is unaffected, but the report does not name the server version."
	report.Limitations = []string{domain.LimitationDiscoveryBoundary, runSpecific, domain.LimitationRemovedTargets, domain.LimitationLocalHelm}
	terminal, err := Encode(report, "terminal", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(terminal), "[?] Coverage: "+runSpecific) || !strings.Contains(string(terminal), "[?] Coverage: 3 standard boundaries") {
		t.Fatalf("run limitation or standing count missing:\n%s", terminal)
	}
	for _, standing := range []string{domain.LimitationDiscoveryBoundary, domain.LimitationRemovedTargets, "Local Helm uses the bundled engine"} {
		if strings.Contains(string(terminal), standing) {
			t.Fatalf("terminal repeats standing limitation %q", standing)
		}
	}
	for _, format := range []string{"markdown", "json"} {
		out, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		for _, limitation := range report.Limitations {
			want := limitation
			if format == "json" {
				encoded, _ := json.Marshal(limitation)
				want = string(encoded)
			}
			if !strings.Contains(string(out), want) {
				t.Fatalf("%s lost limitation %q", format, limitation)
			}
		}
	}
}
