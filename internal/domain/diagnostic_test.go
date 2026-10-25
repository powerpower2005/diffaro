package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestFailRecordsItsSiteAndCauseChainWithoutText(t *testing.T) {
	const canary = "fictional-canary-chain"
	cause := fmt.Errorf("reading %s: %w", canary, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)})
	err := Fail(CodeConnectionFailed, "connection", "request failed", "retry", cause)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Issue.Diagnostic == nil {
		t.Fatalf("no diagnostic: %v", err)
	}
	d := failure.Issue.Diagnostic
	if !strings.HasPrefix(d.Site, "domain/diagnostic_test.go:") {
		t.Fatalf("site = %q, want this test's line", d.Site)
	}
	want := []string{"*fmt.wrapError", "*net.OpError", "*os.SyscallError", "syscall.Errno(" + syscall.ECONNRESET.Error() + ")"}
	if !slices.Equal(d.Chain, want) {
		t.Fatalf("chain = %v, want %v", d.Chain, want)
	}
	encoded, _ := json.Marshal(failure.Issue)
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("diagnostic kept error text: %s", encoded)
	}
}

func TestErrorChainNamesSentinelsJoinedCausesAndFailures(t *testing.T) {
	inner := Fail(CodeNotFound, "lookup", "missing", "check", nil)
	chain := ErrorChain(errors.Join(io.ErrUnexpectedEOF, fmt.Errorf("wrapped: %w", context.DeadlineExceeded), inner))
	for _, link := range []string{"io.ErrUnexpectedEOF", "context.DeadlineExceeded", "*domain.Failure(not_found)"} {
		if !slices.Contains(chain, link) {
			t.Fatalf("chain %v lacks %s", chain, link)
		}
	}
	var long error = io.EOF
	for range 50 {
		long = fmt.Errorf("again: %w", long)
	}
	if got := len(ErrorChain(long)); got != maxChainLinks {
		t.Fatalf("chain length %d, want bound %d", got, maxChainLinks)
	}
}

func TestUnclassifiedIssueIsADefectWithItsChain(t *testing.T) {
	issue := UnclassifiedIssue(errors.New("fictional-canary-untyped"), "preview", "preview could not complete")
	if issue.Code != CodeInternalError || issue.ReasonCode != ReasonUnclassified || issue.Action != DefectAction {
		t.Fatalf("issue = %+v", issue)
	}
	if issue.Diagnostic == nil || !slices.Equal(issue.Diagnostic.Chain, []string{"*errors.errorString"}) {
		t.Fatalf("diagnostic = %+v", issue.Diagnostic)
	}
	encoded, _ := json.Marshal(issue)
	if strings.Contains(string(encoded), "fictional-canary-untyped") {
		t.Fatalf("unclassified issue kept error text: %s", encoded)
	}
}

func TestDiagnosticWithNeverMutatesTheSharedDiagnostic(t *testing.T) {
	shared := &Diagnostic{Site: "a/b.go:1"}
	changed := shared.With(func(d *Diagnostic) { d.Route = "/x" })
	if shared.Route != "" || changed.Route != "/x" || changed.Site != "a/b.go:1" {
		t.Fatalf("shared %+v changed %+v", shared, changed)
	}
}

func TestTraceStepsKeepTheFailureDiagnostic(t *testing.T) {
	trace := NewTrace()
	ctx := WithTrace(context.Background(), trace)
	TraceStart(ctx, "render", "typed", "chart")(Fail(CodeLocalRenderFailed, "render", "failed", "check", io.ErrUnexpectedEOF))
	TraceStart(ctx, "render", "untyped", "chart")(errors.New("fictional-canary-trace"))
	TraceStart(ctx, "render", "ok", "chart")(nil)
	steps, _ := trace.Steps()
	byStep := map[string]TraceStep{}
	for _, step := range steps {
		byStep[step.Step] = step
	}
	if d := byStep["typed"].Diagnostic; d == nil || !strings.HasPrefix(d.Site, "domain/diagnostic_test.go:") || !slices.Equal(d.Chain, []string{"io.ErrUnexpectedEOF"}) {
		t.Fatalf("typed step diagnostic = %+v", d)
	}
	if d := byStep["untyped"].Diagnostic; d == nil || !slices.Equal(d.Chain, []string{"*errors.errorString"}) {
		t.Fatalf("untyped step diagnostic = %+v", d)
	}
	if byStep["ok"].Diagnostic != nil {
		t.Fatal("successful step has a diagnostic")
	}
	encoded, _ := json.Marshal(steps)
	if strings.Contains(string(encoded), "fictional-canary-trace") {
		t.Fatalf("trace kept error text: %s", encoded)
	}
}

// A source failure around an error without a Failure keeps its chain.
func TestSourceFailureOfAnUntypedErrorKeepsItsChain(t *testing.T) {
	err := SourceFailure(io.ErrUnexpectedEOF, SourceRevision{Position: 2, Repository: "https://github.com/example/charts.git", Revision: "main"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Issue.Diagnostic == nil || !slices.Equal(failure.Issue.Diagnostic.Chain, []string{"io.ErrUnexpectedEOF"}) {
		t.Fatalf("failure = %+v", failure)
	}
}

// A defect asks for a bundle to attach to a public bug report, so its action
// names the public level: a profile run mode may select a private bundle.
func TestDefectActionAsksForAPublicBundle(t *testing.T) {
	if !strings.Contains(DefectAction, "--debug-bundle-level public") {
		t.Fatalf("defect action does not request a public bundle: %q", DefectAction)
	}
}
