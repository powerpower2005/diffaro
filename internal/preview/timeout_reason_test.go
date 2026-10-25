package preview

import (
	"context"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

const stalledStep = "Argo CD instance one target manifest rendering (GET /api/v1/applications/{application}/manifests): API request to argocd.example.invalid timed out"

// stalledBackend answers lookups and project checks and stalls every render
// until its stage deadline, failing the way a request adapter does.
type stalledBackend struct{ windowBackend }

func (b *stalledBackend) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	<-ctx.Done()
	return nil, domain.FailReason(domain.CodeConnectionFailed, domain.ReasonTimeout, "connection", stalledStep, "check connectivity", ctx.Err())
}

// A stage deadline keeps the step it stopped and names the configured
// render.timeout; it is still a timeout.
func TestStageTimeoutNamesTheStoppedStepAndConfiguredLimit(t *testing.T) {
	c := windowConfig(1, 1)
	c.Render.Timeout = "50ms"
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": &stalledBackend{}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	Finalize(context.Background(), &report, c, FinalizeOptions{Diff: true})
	if len(report.Applications) != 1 || len(report.Applications[0].Issues) == 0 {
		t.Fatalf("no failed Application: %+v", report.Applications)
	}
	for _, issue := range report.Applications[0].Issues {
		if issue.Code != domain.CodeTimeout || issue.Stage != "connection" || !strings.HasPrefix(issue.Reason, stalledStep) || !strings.HasSuffix(issue.Reason, "; exceeded render.timeout (50ms)") || !strings.Contains(issue.Action, "raise render.timeout") {
			t.Fatalf("timeout lost its step or limit: %+v", issue)
		}
	}
}

// stoppedHelmBackend stalls every render until its stage deadline and then
// fails the way local Helm reports a render its context stopped: the
// Failure already carries the stop code.
type stoppedHelmBackend struct{ windowBackend }

const stoppedHelmRender = "bundled Helm render did not finish"

func (b *stoppedHelmBackend) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	<-ctx.Done()
	return nil, domain.Fail(domain.CodeTimeout, "render", stoppedHelmRender, "rerun the preview, or raise render.timeout if this render needs longer", ctx.Err())
}

// A local Helm render its stage deadline stopped keeps its own reason and
// names the configured render.timeout, although its Failure already has the
// timeout code.
func TestStoppedLocalHelmRenderKeepsItsReasonAndConfiguredLimit(t *testing.T) {
	c := windowConfig(1, 1)
	c.Render.Timeout = "50ms"
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": &stoppedHelmBackend{}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	Finalize(context.Background(), &report, c, FinalizeOptions{Diff: true})
	if len(report.Applications) != 1 || len(report.Applications[0].Issues) == 0 {
		t.Fatalf("no failed Application: %+v", report.Applications)
	}
	for _, issue := range report.Applications[0].Issues {
		if issue.Code != domain.CodeTimeout || issue.Stage != "render" || issue.Reason != stoppedHelmRender+"; exceeded render.timeout (50ms)" {
			t.Fatalf("stopped render lost its reason or limit: %+v", issue)
		}
	}
}

// A deadline or cancellation without a named step, or one already worded as
// a cancellation, keeps the generic reason; a cancelled step keeps its own.
func TestStoppedIssuesWithoutAStepKeepTheGenericReason(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	for _, tc := range []struct {
		name         string
		err          error
		code, reason string
	}{
		{"raw deadline", context.DeadlineExceeded, domain.CodeTimeout, "a preview step exceeded render.timeout"},
		{"raw cancellation", context.Canceled, domain.CodeCancelled, "preview was cancelled"},
		{"cancellation failure", domain.Fail(domain.CodeCancelled, "render", "preview was cancelled", "rerun the preview", context.Canceled), domain.CodeCancelled, "preview was cancelled"},
		{"cancelled local Helm render", domain.Fail(domain.CodeCancelled, "render", stoppedHelmRender, "rerun the preview", context.Canceled), domain.CodeCancelled, stoppedHelmRender + "; the run was cancelled before this step finished"},
		{"cancelled step", domain.Fail(domain.CodeConnectionFailed, "connection", "GitHub file read (GET /repos/{owner}/{repo}/contents/{path}): request failed", "check connectivity", context.Canceled), domain.CodeCancelled, "GitHub file read (GET /repos/{owner}/{repo}/contents/{path}): request failed; the run was cancelled before this step finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if issue := ContextualIssue(tc.err, id, "repo", "sha"); issue.Code != tc.code || issue.Reason != tc.reason {
				t.Fatalf("got %s %q, want %s %q", issue.Code, issue.Reason, tc.code, tc.reason)
			}
		})
	}
	// Finalize names the limit only on timeout reasons that end with it.
	report := domain.Report{Issues: []domain.Issue{{Code: domain.CodeTimeout, Reason: "a preview step exceeded render.timeout"}, {Code: domain.CodeCancelled, Reason: "preview was cancelled"}}}
	nameTimeoutLimit(&report, "4s")
	if report.Issues[0].Reason != "a preview step exceeded render.timeout (4s)" || report.Issues[1].Reason != "preview was cancelled" {
		t.Fatalf("limit named wrongly: %+v", report.Issues)
	}
}
