package domain

import (
	"errors"
	"strings"
	"testing"
)

//go:noinline
func indexPastEnd(items []int) int { return items[len(items)] }

func recovered(fn func()) (err error) {
	defer RecoverAs(&err, "render")
	fn()
	return nil
}

// A recovered panic becomes an internal_error failure whose stack starts at
// the frame that panicked, below the runtime's panic handling.
func TestRecoverAsKeepsThePanickingFrame(t *testing.T) {
	err := recovered(func() { indexPastEnd([]int{1}) })
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("panic was not converted: %v", err)
	}
	issue := failure.Issue
	if issue.Code != CodeInternalError || issue.ReasonCode != ReasonPanic || issue.Stage != "render" || !strings.Contains(issue.Action, "Diffaro bug") {
		t.Fatalf("issue = %+v", issue)
	}
	if !strings.Contains(issue.Reason, "index out of range") {
		t.Fatalf("a runtime error keeps the runtime's message: %q", issue.Reason)
	}
	stack := issue.Diagnostic.Stack
	if len(stack) == 0 || !strings.HasPrefix(stack[0], "diffaro/internal/domain.indexPastEnd domain/panic_test.go:") {
		t.Fatalf("stack must start at the panicking function: %v", stack)
	}
	for _, frame := range stack {
		if strings.HasPrefix(frame, "runtime.") || strings.HasPrefix(frame, "diffaro/internal/domain.RecoverAs ") || strings.Contains(frame, "/Users/") {
			t.Fatalf("stack holds recovery or runtime frames or a build path: %v", stack)
		}
	}
}

// A panic value that is not a runtime error can carry input data; only its
// type is reported.
func TestRecoverAsWithholdsPanicValues(t *testing.T) {
	for _, value := range []any{"token=fictional-secret-value", errors.New("fictional-secret-value"), map[string]string{"password": "fictional-secret-value"}} {
		err := recovered(func() { panic(value) })
		var failure *Failure
		if !errors.As(err, &failure) || strings.Contains(failure.Issue.Reason, "fictional-secret-value") || !strings.Contains(failure.Issue.Reason, "Diffaro panicked: a ") {
			t.Fatalf("panic value leaked or was not named by type: %+v", failure)
		}
	}
}

func recurse(n int) int {
	if n == 0 {
		var m map[string]int
		m["x"] = 1
	}
	return recurse(n-1) + 1
}

func TestRecoveredStackIsBounded(t *testing.T) {
	err := recovered(func() { recurse(100) })
	var failure *Failure
	if !errors.As(err, &failure) || len(failure.Issue.Diagnostic.Stack) != maxPanicFrames {
		t.Fatalf("stack frames = %d, want %d", len(failure.Issue.Diagnostic.Stack), maxPanicFrames)
	}
}

func TestRecoverAsLeavesNormalReturns(t *testing.T) {
	if err := recovered(func() {}); err != nil {
		t.Fatal(err)
	}
}
