package debugbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// canaries are names, text and values a public bundle must never hold.
var canaries = []string{"canary-instance", "canary-namespace", "canary-app", "canary-repo", "canary-revision", "canary-project-subject", "CANARY_TOKEN_ENV", "canary-reason", "canary-action", "canary-limitation", "canary-skip", "canary-digest", "canary-value"}

func fixture() Input {
	id := domain.ApplicationID{Instance: "canary-instance", Namespace: "canary-namespace", Name: "canary-app"}
	failure := domain.Issue{Code: domain.CodeAccessDenied, ReasonCode: domain.ReasonRequestForbidden, Stage: "permission", Side: "head", Reason: "canary-reason", Action: "canary-action", Repository: "canary-repo", Revision: "canary-revision", Application: &id,
		RequiredAccess: &domain.AccessRequirement{System: "argocd", Instance: "canary-instance", CredentialEnv: "CANARY_TOKEN_ENV", Resource: "projects", Action: "get", Target: "canary-project-subject"},
		Diagnostic:     &domain.Diagnostic{Site: "httpx/diagnostic.go:72", HTTPStatus: 403, Route: "/api/v1/projects/{name}"}}
	report := domain.NewReport()
	report.Status = domain.StatusPartialFailure
	report.Revisions = domain.RevisionPair{Repository: "canary-repo", Base: "canary-revision", Head: "canary-revision-head"}
	report.Limitations = []string{"canary-limitation"}
	report.Generator = &domain.Generator{Version: "dev", Command: "diff", Profile: domain.ProfileShape{Instances: 1, Backends: []string{"argocd-application-api"}}}
	report.RenderBackends = []domain.BackendInfo{{Instance: "canary-instance", Kind: "argocd-application-api", Version: "v3.5.3", Verification: "live-unverified"}}
	report.Discovery = []domain.DiscoveryResult{{Instance: "canary-instance", Status: "complete", Decisions: []domain.DiscoveryDecision{{ID: id, Project: "canary-project-subject", Selected: true, Reason: "canary-reason", Selection: "rendered", SelectionReason: "canary-reason"}}}}
	report.Applications = []domain.ApplicationResult{{ID: id, State: domain.StateUnknown, Execution: domain.ExecutionFailed, SkipReason: "canary-skip", Issues: []domain.Issue{failure},
		Resources: []domain.ResourceDiff{{Changes: []domain.Change{{Path: "/data/key", Before: "canary-value", After: "canary-value-2"}}}}}}
	report.Issues = []domain.Issue{failure}
	steps := []domain.TraceStep{
		{Seq: 1, Application: &id, Side: "head", Revision: "canary-revision", Walk: "explicit", Phase: "project", Step: "check", Subject: "canary-project-subject", Outcome: "failed", Code: domain.CodeAccessDenied, Reason: "canary-reason", Digest: "sha256:canary-digest", Diagnostic: failure.Diagnostic},
		{Seq: 2, Application: &id, Phase: "http", Step: "Argo CD project", Subject: "/api/v1/projects/{name}", Outcome: "failed", Status: 403},
	}
	return Input{Level: LevelPublic, Report: report, ExitCode: domain.ExitPartialFailure, Flags: []string{"config", "pr", "debug-bundle"}, Steps: steps, Key: []byte("fictional-fixed-key")}
}

func unpack(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Mode != 0o600 || !strings.HasPrefix(header.Name, "diffaro-debug-bundle/") {
			t.Fatalf("entry %s mode %o", header.Name, header.Mode)
		}
		data, _ := io.ReadAll(tr)
		files[strings.TrimPrefix(header.Name, "diffaro-debug-bundle/")] = string(data)
	}
}

// A public bundle keeps codes, stages, diagnostics, counts and trace
// structure, and holds no name, free text, value or digest; the names file
// maps every pseudonym back.
func TestPublicBundleHoldsStructureWithoutNamesOrText(t *testing.T) {
	out, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	files := unpack(t, out.Bundle)
	if len(files) != 2 || files["README.md"] == "" || files["bundle.json"] == "" {
		t.Fatalf("files = %v", keys(files))
	}
	all := files["README.md"] + files["bundle.json"]
	for _, canary := range canaries {
		if strings.Contains(all, canary) {
			t.Fatalf("public bundle holds %q:\n%s", canary, files["bundle.json"])
		}
	}
	var s summary
	if err := json.Unmarshal([]byte(files["bundle.json"]), &s); err != nil {
		t.Fatal(err)
	}
	app, issue := s.Applications[0], s.Applications[0].Issues[0]
	if s.Schema != Schema || s.ExitCode != domain.ExitPartialFailure || s.Generator == nil || s.Counts.IssueCodes[domain.CodeAccessDenied] != 1 || s.Counts.Resources != 1 || s.RenderBackends[0].Version != "v3.5.3" {
		t.Fatalf("summary = %+v", s)
	}
	if issue.Code != domain.CodeAccessDenied || issue.ReasonCode != domain.ReasonRequestForbidden || issue.Diagnostic == nil || issue.Diagnostic.Route != "/api/v1/projects/{name}" || issue.RequiredAccess.Resource != "projects" {
		t.Fatalf("issue = %+v", issue)
	}
	// One name gets one pseudonym everywhere in the bundle.
	if !strings.HasPrefix(app.ID.Name, "app-") || *issue.Application != *app.ID || *s.Discovery[0].Decisions[0].ID != *app.ID || *s.Trace.Steps[0].Application != *app.ID || s.Trace.Steps[0].Revision != issue.Revision {
		t.Fatalf("pseudonyms differ: app %+v issue %+v", app.ID, issue)
	}
	if s.Trace.Steps[1].Subject != "/api/v1/projects/{name}" || !strings.HasPrefix(s.Trace.Steps[0].Subject, "subject-") {
		t.Fatalf("trace subjects = %q, %q", s.Trace.Steps[0].Subject, s.Trace.Steps[1].Subject)
	}
	var names struct {
		Names map[string]string `json:"names"`
	}
	if err := json.Unmarshal(out.Names, &names); err != nil || names.Names[app.ID.Name] != "canary-app" || names.Names[issue.RequiredAccess.CredentialEnv] != "CANARY_TOKEN_ENV" || names.Names[s.Revisions.Base] != "canary-revision" {
		t.Fatalf("names = %v (%v)", names.Names, err)
	}
}

// The same key gives the same bundle; a fresh key gives other pseudonyms,
// so pseudonyms cannot be matched across bundles.
func TestBundlePseudonymsAreKeyedPerBundle(t *testing.T) {
	first, _ := Build(fixture())
	second, _ := Build(fixture())
	if !bytes.Equal(first.Bundle, second.Bundle) {
		t.Fatal("equal input and key gave different bundles")
	}
	unkeyed := fixture()
	unkeyed.Key = nil
	a, _ := Build(unkeyed)
	b, _ := Build(unkeyed)
	if bytes.Equal(a.Names, b.Names) || bytes.Equal(a.Names, first.Names) {
		t.Fatal("bundles without a key share pseudonyms")
	}
}

// A private bundle keeps names and adds the redacted report and trace; it
// has no names file.
func TestPrivateBundleKeepsNamesAndAddsReportAndTrace(t *testing.T) {
	in := fixture()
	in.Level, in.ReportJSON, in.TraceJSON = LevelPrivate, []byte(`{"report":true}`), []byte(`{"trace":true}`)
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	files := unpack(t, out.Bundle)
	if out.Names != nil || files["report.json"] != `{"report":true}` || files["trace.json"] != `{"trace":true}` || !strings.Contains(files["bundle.json"], `"canary-app"`) {
		t.Fatalf("private bundle: names=%s files=%v", out.Names, keys(files))
	}
	// Free text stays out at every level; it is in report.json.
	if strings.Contains(files["bundle.json"], "canary-reason") {
		t.Fatal("bundle summary holds free text")
	}
	if _, err := Build(Input{Level: "everything"}); err == nil {
		t.Fatal("unknown level accepted")
	}
}

// counts.issueCodes counts every distinct issue once: an issue only on an
// Application, as in an explicit run, and an issue mirrored at run level, as
// a graph fault is.
func TestIssueCodesCountApplicationIssuesOnce(t *testing.T) {
	render := domain.Issue{Code: domain.CodeLocalRenderFailed, ReasonCode: domain.ReasonTemplateFailed, Stage: "render", Side: "head"}
	for _, tc := range []struct {
		name     string
		runLevel bool
	}{{"application only", false}, {"mirrored at run level", true}} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			in.Report.Applications[0].Issues = []domain.Issue{render}
			in.Report.Issues = nil
			if tc.runLevel {
				in.Report.Issues = []domain.Issue{render}
			}
			out, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			var s summary
			if err := json.Unmarshal([]byte(unpack(t, out.Bundle)["bundle.json"]), &s); err != nil {
				t.Fatal(err)
			}
			if len(s.Counts.IssueCodes) != 1 || s.Counts.IssueCodes[domain.CodeLocalRenderFailed] != 1 {
				t.Fatalf("issueCodes = %v, want local_render_failed once", s.Counts.IssueCodes)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := []string{}
	for key := range m {
		out = append(out, key)
	}
	return out
}
