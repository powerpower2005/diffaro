package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// focusReport has one changed Deployment, one Application that failed the
// same way at base and head, and changed-path selection decisions.
func focusReport() domain.Report {
	r := tailChangeReport("a", "b")
	r.Status = "partial_failure"
	failed := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "removed-chart"}
	app := domain.ApplicationResult{ID: failed, State: "unknown", Execution: "failed", FailureOrigin: "preexisting"}
	for _, side := range []struct{ name, revision string }{{"base", strings.Repeat("a", 40)}, {"head", strings.Repeat("b", 40)}} {
		app.Issues = append(app.Issues, domain.Issue{Code: "not_found", Stage: "render", Reason: "source path is missing", Action: "check the path", Application: &failed, Side: side.name, Revision: side.revision})
	}
	r.Applications = append(r.Applications, app)
	r.Discovery = []domain.DiscoveryResult{{Instance: "east", Status: "complete", Unaffected: 1, Decisions: []domain.DiscoveryDecision{
		{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, Selected: true, Selection: "rendered", SelectionReason: "PR changes files under the source path"},
		{ID: failed, Selected: true, Selection: "ineligible", SelectionReason: "Argo CD status does not show whether this Application manages child Applications"},
		{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "other"}, Selected: true, Selection: "unaffected", SelectionReason: "no PR file is under the source path"},
	}}}
	r.RenderBackends = []domain.BackendInfo{{Instance: "east", Kind: "local-helm", Compatibility: "bounded-git-helm-external"}}
	return r
}

func TestPRMarkdownShowsTargetChangesBeforeCollapsedEvidence(t *testing.T) {
	data, err := Encode(focusReport(), "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	visible := string(data)[:strings.Index(string(data), "<summary>How this was compared")]
	if !strings.Contains(visible, "**changed** Deployment `demo/api`") || !strings.Contains(visible, "~~~diff\n@@ /spec/template/spec/containers/0/args @@\n-a\n+b\n~~~") {
		t.Fatalf("changed fields are not in the visible part:\n%s", visible)
	}
	if strings.Count(string(data), "/spec/template/spec/containers/0/args") != 1 {
		t.Fatal("changed fields are repeated in the evidence")
	}
}

// TestIssueAtBaseAndHeadIsShownOnce checks the merge step itself: a failure
// at base and head must read exactly as often as the same failure at base
// only, with both sides and revisions named.
func TestIssueAtBaseAndHeadIsShownOnce(t *testing.T) {
	bothSides := focusReport()
	baseOnly := focusReport()
	for i := range baseOnly.Applications {
		app := &baseOnly.Applications[i]
		if len(app.Issues) == 2 {
			app.Issues = app.Issues[:1]
		}
	}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		both, err := Encode(bothSides, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		single, err := Encode(baseOnly, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		reason := "source path is missing"
		if strings.Count(string(both), reason) != strings.Count(string(single), reason) || !strings.Contains(string(single), reason) {
			t.Fatalf("%s repeats a failure per side: %d vs %d", format, strings.Count(string(both), reason), strings.Count(string(single), reason))
		}
		out := string(both)
		if !strings.Contains(out, "base, head") || !strings.Contains(out, strings.Repeat("a", 40)+", "+strings.Repeat("b", 40)) {
			t.Fatalf("%s lost the merged sides or revisions:\n%s", format, out)
		}
		if !strings.Contains(out, "already failing before this PR") {
			t.Fatalf("%s does not say the failure predates the PR:\n%s", format, out)
		}
	}
}

func TestSelectionReasonsAndEmptyValuesInMarkdown(t *testing.T) {
	for _, format := range []string{"markdown", "pr-markdown"} {
		data, err := Encode(focusReport(), format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{
			"- `east/argocd/api`: rendered (PR changes files under the source path)",
			"- `east/argocd/removed-chart`: ineligible (Argo CD status does not show whether this Application manages child Applications)",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks selection reason %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "east/argocd/other`: unaffected") {
			t.Fatalf("%s lists unaffected candidates one by one", format)
		}
		if strings.Contains(out, "``") || strings.Contains(out, "Version: ") {
			t.Fatalf("%s prints an empty value as a code span:\n%s", format, out)
		}
	}
}

func TestExcludedPreexistingFailuresAreStatedNextToTheStatus(t *testing.T) {
	r := focusReport()
	r.Status, r.PreexistingFailuresExcluded = "success", 1
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		notice := strings.Index(out, "Not counted: 1 Application(s) already failing before this PR (failures.excludePreexistingFromExit)")
		if notice < 0 || !strings.Contains(out, "source path is missing") {
			t.Fatalf("%s hides the excluded failure:\n%s", format, out)
		}
		// The notice comes before the details of any Application.
		if details := strings.Index(out, "removed-chart"); details >= 0 && details < notice {
			t.Fatalf("%s states the exclusion after the Application details", format)
		}
	}
}

// A candidate skipped because its source path exists at neither revision is
// named with its reason; ordinary unaffected candidates are only counted.
func TestSkippedMissingSourceIsNamedWithItsReason(t *testing.T) {
	r := focusReport()
	missing := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "orphan"}
	r.Discovery[0].Unaffected = 2
	r.Discovery[0].Decisions = append(r.Discovery[0].Decisions, domain.DiscoveryDecision{ID: missing, Selected: true, Selection: "source-missing", SelectionReason: "source path charts/orphan/ does not exist at base or head and no PR file is under it; the Application renders nothing on either revision"})
	for _, format := range []string{"markdown", "pr-markdown"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"1 unaffected and not rendered", "1 not rendered because their source path exists at neither revision", "`east/argocd/orphan`: source-missing (source path charts/orphan/ does not exist at base or head"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
	}
}
