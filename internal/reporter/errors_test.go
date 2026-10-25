package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// originFailure is a failed Application with one issue at base and head,
// listed on the Application and at report level as preview reports do.
func originFailure(name, origin, code, reasonCode, base, head string) (domain.ApplicationResult, []domain.Issue) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}
	app := domain.ApplicationResult{ID: id, State: "unknown", Execution: "failed", FailureOrigin: domain.FailureOrigin(origin),
		BaseTarget: &domain.TargetSummary{Execution: domain.Execution(base)}, HeadTarget: &domain.TargetSummary{Execution: domain.Execution(head)}}
	for _, side := range []struct{ name, execution, revision string }{{"base", base, strings.Repeat("a", 40)}, {"head", head, strings.Repeat("b", 40)}} {
		if side.execution == "failed" {
			app.Issues = append(app.Issues, domain.Issue{Code: code, ReasonCode: reasonCode, Stage: "render", Reason: name + " cannot be rendered", Action: "check " + name, Application: &id, Side: side.name, Revision: side.revision})
		}
	}
	return app, append([]domain.Issue(nil), app.Issues...)
}

func failureReport(apps ...func() (domain.ApplicationResult, []domain.Issue)) domain.Report {
	r := domain.NewReport()
	r.Status = "partial_failure"
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	for _, build := range apps {
		app, issues := build()
		r.Applications = append(r.Applications, app)
		r.Issues = append(r.Issues, issues...)
	}
	return r
}

func prMarkdown(t *testing.T, r domain.Report) string {
	t.Helper()
	data, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The Errors section names each cause's origin and states that the failed
// Applications' resource changes are unknown, not unchanged.
func TestErrorsSectionStatesOriginAndEffect(t *testing.T) {
	for _, tc := range []struct {
		name, origin, base, head, effect string
	}{
		{"preexisting", "preexisting", "failed", "failed", "- **Effect:** resource changes of these Applications are unknown, not unchanged."},
		{"introduced", "introduced", "complete", "failed", "- **Effect:** resource changes of these Applications are unknown, not unchanged."},
		{"resolved", "resolved", "failed", "complete", "- **Effect:** head targets are available, but base targets are unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := prMarkdown(t, failureReport(func() (domain.ApplicationResult, []domain.Issue) {
				return originFailure("api", tc.origin, "not_found", "chart_path_missing", tc.base, tc.head)
			}))
			// PR Markdown keeps the full causes in collapsed details.
			_, errors, ok := strings.Cut(out, "<summary>Error details: where, why and what to do</summary>")
			if !ok {
				t.Fatalf("no error details:\n%s", out)
			}
			// Markdown escapes the label (an apostrophe becomes an entity).
			if want := "- **Origin:** " + prText(originLabels[domain.FailureOrigin(tc.origin)], 240); !strings.Contains(errors, want) {
				t.Fatalf("missing %q in Errors section:\n%s", want, errors)
			}
			if !strings.Contains(errors, tc.effect) {
				t.Fatalf("missing %q in Errors section:\n%s", tc.effect, errors)
			}
		})
	}
}

// Failures introduced by the PR are listed before failures that predate it,
// whatever their codes' alphabetical order.
func TestErrorsSectionListsIntroducedBeforePreexisting(t *testing.T) {
	out := prMarkdown(t, failureReport(
		func() (domain.ApplicationResult, []domain.Issue) {
			return originFailure("old", "preexisting", "local_render_failed", "", "failed", "failed")
		},
		func() (domain.ApplicationResult, []domain.Issue) {
			return originFailure("new", "introduced", "render_failed", "", "complete", "failed")
		},
	))
	introduced := strings.Index(out, "### Render failed (`render_failed`)")
	preexisting := strings.Index(out, "### Local render failed (`local_render_failed`)")
	if introduced < 0 || preexisting < 0 {
		t.Fatalf("cause headings missing:\n%s", out)
	}
	if introduced > preexisting {
		t.Fatalf("a preexisting failure is listed before one introduced by the PR:\n%s", out)
	}
}

// A failure at a revision resolved from a branch at run time names that
// branch: a chart removed from the branch after the PR fails today's render,
// and the report must not read as if the PR broke it.
func TestFailureAtResolvedRevisionNamesItsBranch(t *testing.T) {
	sha := strings.Repeat("c", 40)
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "removed"}
	r := domain.NewReport()
	r.Status = domain.StatusPartialFailure
	r.Revisions = domain.RevisionPair{Repository: "example/states", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	r.Applications = []domain.ApplicationResult{{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, State: domain.StateRemoved, Execution: domain.ExecutionFailed, Resources: []domain.ResourceDiff{},
		BaseTarget: &domain.TargetSummary{Repository: "https://git.example.com/charts", Revision: sha, RevisionRule: "resolved-source-reference", RequestedRevision: "release/live", Execution: domain.ExecutionFailed},
		Issues:     []domain.Issue{{Code: domain.CodeLocalRenderFailed, ReasonCode: domain.ReasonChartPathMissing, Stage: "render", Side: "base", Repository: "https://git.example.com/charts", Revision: sha, Reason: "chart path does not exist at this revision", Action: "check the path", Application: &id}}}}
	for format, want := range map[string]string{
		"pr-markdown": "revision `" + sha + "` (resolved from `release/live` at run time)",
		"markdown":    "revision `" + sha + "` (resolved from `release/live` at run time)",
		"terminal":    sha + " (resolved from release/live at run time)",
	} {
		if out := encodeText(t, r, format); !strings.Contains(out, want) {
			t.Fatalf("%s does not name the resolved branch %q:\n%s", format, want, out)
		}
	}
}
