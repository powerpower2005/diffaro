package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// A panic outside every Application boundary still yields a failed report,
// so the report, trace and exit code are written as for any failed run. A
// nil provider client panics here like any defect would.
func TestPreviewPanicBecomesAFailedReport(t *testing.T) {
	o := &commandOptions{command: "doctor"}
	in := runInputs{config: profile.Config{Git: profile.Git{Repository: "example/charts"}}}
	report := runPreview(context.Background(), o, in, &backends{}, startProgress(io.Discard, true, time.Hour))
	if report.Status != domain.StatusFailed || report.ExitCode() != domain.ExitFailed || len(report.Issues) != 1 {
		t.Fatalf("status=%s issues=%+v", report.Status, report.Issues)
	}
	issue := report.Issues[0]
	if issue.Code != domain.CodeInternalError || issue.ReasonCode != domain.ReasonPanic || issue.Diagnostic == nil || len(issue.Diagnostic.Stack) == 0 || issue.Repository != "example/charts" {
		t.Fatalf("issue = %+v", issue)
	}
}

type panickingWriter struct{}

func (panickingWriter) Write([]byte) (int, error) {
	var buffers []*bytes.Buffer
	return buffers[0].Len(), nil
}

// A panic that escapes the comparison, here while writing output, ends with
// a bug notice that names the build and the stack, and a failure exit code.
func TestRunTurnsAnEscapedPanicIntoAFailureExit(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--version"}, panickingWriter{}, &stderr); code != domain.ExitFailed {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Diffaro internal error. Diffaro ", "internal_error [stage=cli]", "Cause: panic", "Diffaro bug", "  Stack:\n    diffaro/internal/cli.panickingWriter.Write "} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
}

// tickPanickingWriter panics only on periodic "still ..." progress lines,
// which the progress goroutine writes.
type tickPanickingWriter struct{}

func (tickPanickingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "still") {
		return panickingWriter{}.Write(p)
	}
	return len(p), nil
}

// Periodic progress lines are optional: a panic while writing one stops them
// without crashing the run or leaving the progress lock held.
func TestProgressSurvivesAPanickingWriter(t *testing.T) {
	p := startProgress(tickPanickingWriter{}, false, time.Millisecond)
	p.stage("rendering")
	deadline := time.After(5 * time.Second)
	select {
	case <-p.done:
	case <-deadline:
		t.Fatal("progress goroutine did not end after its writer panicked")
	}
	p.stage("reporting")
	p.close()
}
