package preview

import (
	"context"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestFailureOriginComparesBaseAndHead(t *testing.T) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	missing := func(side string) domain.Issue {
		return domain.Issue{Code: "local_render_failed", ReasonCode: "chart_path_missing", Stage: "render", Reason: `chart path "charts/api" does not exist at this revision`, Side: side, Revision: side + "-sha", Application: &id}
	}
	app := func(base, head string, issues ...domain.Issue) domain.ApplicationResult {
		return domain.ApplicationResult{ID: id, BaseTarget: &domain.TargetSummary{Execution: domain.Execution(base)}, HeadTarget: &domain.TargetSummary{Execution: domain.Execution(head)}, Issues: issues}
	}
	timeout := missing("head")
	timeout.Code, timeout.ReasonCode = "connection_failed", ""
	// A stage deadline is recorded with code timeout (issues.go).
	deadline := missing("head")
	deadline.Code, deadline.ReasonCode = "timeout", ""
	otherCause := missing("head")
	otherCause.Reason = "a different reason"
	denied := domain.Issue{Code: "access_denied", Stage: "permission", Reason: "forbidden", Side: "base"}
	deniedHead := denied
	deniedHead.Side = "head"
	withInputs := app("failed", "failed", missing("base"), missing("head"))
	withInputs.InputChanges = []domain.InputChange{{Kind: "valueFile", Path: "charts/api/values.yaml"}}
	for _, tc := range []struct {
		name      string
		app       domain.ApplicationResult
		unchanged bool
		want      domain.FailureOrigin
	}{
		{"head breaks", app("complete", "failed", missing("head")), true, domain.FailureIntroduced},
		{"head fixes", app("failed", "complete", missing("base")), true, domain.FailureResolved},
		{"same cause and proven unchanged inputs", app("failed", "failed", missing("base"), missing("head")), true, domain.FailurePreexisting},
		{"same cause without proof", app("failed", "failed", missing("base"), missing("head")), false, domain.FailureBothSides},
		{"same cause and the PR changes an input it reads", withInputs, true, domain.FailureInputsChanged},
		{"different causes", app("failed", "failed", missing("base"), otherCause), true, ""},
		{"retryable failure", app("failed", "failed", missing("base"), timeout), true, domain.FailureTransient},
		{"head-only stage timeout", app("complete", "failed", deadline), true, domain.FailureTransient},
		// Access and configuration failures keep their own exit contract.
		{"denied on both sides is not deterministic", app("failed", "failed", denied, deniedHead), true, domain.FailureBothSides},
		{"no failure", app("complete", "complete"), true, ""},
		{"skipped side", app("skipped", "failed", missing("head")), true, ""},
		{"explicit run without target summaries", domain.ApplicationResult{ID: id, Issues: []domain.Issue{missing("base")}}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureOrigin(tc.app, tc.unchanged); got != tc.want {
				t.Fatalf("origin=%q want %q", got, tc.want)
			}
		})
	}
}

func TestAnnotateFailureOriginsReadsDiscoveryProof(t *testing.T) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	issue := func(side string) domain.Issue {
		return domain.Issue{Code: "not_found", ReasonCode: "chart_path_missing", Stage: "render", Reason: "missing", Side: side, Application: &id}
	}
	report := domain.NewReport()
	report.Status = "partial_failure"
	report.Applications = []domain.ApplicationResult{{ID: id, State: "unknown", Execution: "failed", BaseTarget: &domain.TargetSummary{Execution: "failed"}, HeadTarget: &domain.TargetSummary{Execution: "failed"}, Issues: []domain.Issue{issue("base"), issue("head")}}}
	report.Discovery = []domain.DiscoveryResult{{Instance: "east", Decisions: []domain.DiscoveryDecision{{ID: id, Selected: true, Selection: selectionIneligible, InputsUnchanged: true}}}}
	AnnotateFailureOrigins(&report)
	if got := report.Applications[0]; got.FailureOrigin != domain.FailurePreexisting || got.State != "unknown" || got.Execution != "failed" || report.Status != "partial_failure" || report.ExitCode() != 3 {
		t.Fatalf("origin or outcome changed: %+v", got)
	}
}

func TestInputsUnchangedIgnoresChildApplicationStatus(t *testing.T) {
	read := func(_ context.Context, file string) ([]byte, bool, error) {
		if file == "charts/api/Chart.yaml" {
			return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
		}
		return nil, false, nil
	}
	app := pathApp(helmSource("charts/api", nil))
	for _, tc := range []struct {
		name  string
		files []string
		want  bool
	}{
		{"PR changes other charts", []string{"charts/web/values.yaml"}, true},
		{"PR changes this chart", []string{"charts/api/values.yaml"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := newChangedPathSelector(ChangedPaths{Files: tc.files, Read: read, Listed: true}, profile.Git{RepositoryURL: pathRepo}.Matches)
			// Unknown child status keeps the candidate rendered.
			if selection, _ := selector.decide(context.Background(), app, nil); selection == selectionUnaffected {
				t.Fatal("unknown child status skipped the candidate")
			}
			if got := selector.inputsUnchanged(context.Background(), app); got != tc.want {
				t.Fatalf("inputsUnchanged=%t want %t", got, tc.want)
			}
		})
	}
}

func TestInputsUnchangedForAnUntouchedMissingChart(t *testing.T) {
	// The chart directory was removed before the PR: no Chart.yaml at head.
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	app := pathApp(helmSource("charts/removed", nil))
	for _, tc := range []struct {
		name  string
		files []string
		want  bool
	}{
		{"PR does not touch the directory", []string{"charts/web/values.yaml"}, true},
		// A PR that deletes the chart lists its files, so base had them.
		{"PR deletes the chart", []string{"charts/removed/Chart.yaml"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := newChangedPathSelector(ChangedPaths{Files: tc.files, Read: read, Listed: true}, profile.Git{RepositoryURL: pathRepo}.Matches)
			no := false
			if selection, _ := selector.decide(context.Background(), app, &no); selection == selectionUnaffected {
				t.Fatal("a candidate without Chart.yaml was skipped instead of rendered to report its failure")
			}
			if got := selector.inputsUnchanged(context.Background(), app); got != tc.want {
				t.Fatalf("inputsUnchanged=%t want %t", got, tc.want)
			}
		})
	}
}

func TestExcludePreexistingFailuresOnlyForPartialRunsWithOtherResults(t *testing.T) {
	old := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "old"}
	other := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "other"}
	issueFor := func(id domain.ApplicationID) domain.Issue {
		return domain.Issue{Code: "local_render_failed", Stage: "render", Reason: "missing", Application: &id}
	}
	build := func(status string, apps ...domain.ApplicationResult) domain.Report {
		r := domain.NewReport()
		r.Status, r.ChangesFound = domain.Status(status), true
		r.Applications = apps
		for _, app := range apps {
			r.Issues = append(r.Issues, app.Issues...)
		}
		return r
	}
	preexisting := domain.ApplicationResult{ID: old, Execution: "failed", FailureOrigin: domain.FailurePreexisting, Issues: []domain.Issue{issueFor(old)}}
	changed := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, State: "changed", Execution: "complete"}
	bothSides := domain.ApplicationResult{ID: other, Execution: "failed", FailureOrigin: domain.FailureBothSides, Issues: []domain.Issue{issueFor(other)}}
	runIssue := build("partial_failure", preexisting, changed)
	runIssue.Issues = append(runIssue.Issues, domain.Issue{Code: "selection_changed", Stage: "selection", Reason: "inventory changed"})
	for _, tc := range []struct {
		name     string
		report   domain.Report
		status   string
		exit     int
		excluded int
	}{
		{"only preexisting failures beside a change", build("partial_failure", preexisting, changed), "success", 1, 1},
		{"another failure keeps the partial failure", build("partial_failure", preexisting, changed, bothSides), "partial_failure", 3, 1},
		{"a run-level issue keeps the partial failure", runIssue, "partial_failure", 3, 1},
		{"no useful result stays failed", build("failed", preexisting), "failed", 6, 1},
		{"no preexisting failure", build("partial_failure", changed, bothSides), "partial_failure", 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := tc.report
			apps := len(report.Applications)
			ExcludePreexistingFailures(&report)
			if string(report.Status) != tc.status || report.ExitCode() != tc.exit || report.PreexistingFailuresExcluded != tc.excluded || len(report.Applications) != apps {
				t.Fatalf("status=%s exit=%d excluded=%d apps=%d", report.Status, report.ExitCode(), report.PreexistingFailuresExcluded, len(report.Applications))
			}
			noticed := false
			for _, limit := range report.Limitations {
				noticed = noticed || strings.Contains(limit, "already failing before this PR")
			}
			if noticed != (tc.excluded > 0) {
				t.Fatalf("notice present=%t, want %t", noticed, tc.excluded > 0)
			}
		})
	}
}

// TestCandidateWithoutSourcePathIsNotRendered verifies that a candidate
// whose source path has no file at head and is untouched by the PR renders
// nothing on either revision, so it is skipped with its reason, even when
// Argo CD status cannot show whether it manages child Applications.
func TestCandidateWithoutSourcePathIsNotRendered(t *testing.T) {
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	app := pathApp(helmSource("charts/removed", nil))
	present := map[string]bool{"charts/api/Chart.yaml": true}
	for _, tc := range []struct {
		name  string
		paths ChangedPaths
		want  string
	}{
		{"path absent at head and untouched", ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read, Listed: true, Present: present}, selectionSourceMissing},
		// A PR that deletes the chart lists its files: base still had it,
		// so the candidate is rendered (unknown child status: ineligible).
		{"PR deletes the chart", ChangedPaths{Files: []string{"charts/removed/Chart.yaml"}, Read: read, Listed: true, Present: present}, selectionIneligible},
		{"no tree listing to prove absence", ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read}, selectionIneligible},
		{"path present at head", ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read, Listed: true, Present: map[string]bool{"charts/removed/Chart.yaml": true}}, selectionIneligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := newChangedPathSelector(tc.paths, profile.Git{RepositoryURL: pathRepo}.Matches)
			got, reason := selector.decide(context.Background(), app, nil)
			if got != tc.want || reason == "" {
				t.Fatalf("selection=%s (%s), want %s", got, reason, tc.want)
			}
			if got == selectionSourceMissing && !strings.Contains(reason, "charts/removed/ does not exist at base or head") {
				t.Fatalf("reason does not name the missing path: %s", reason)
			}
		})
	}
}
