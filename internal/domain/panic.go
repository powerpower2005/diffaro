package domain

import (
	"fmt"
	"runtime"
	"strings"
)

// maxPanicFrames bounds a recovered stack.
const maxPanicFrames = 32

// PanicFailure turns a recovered panic value into an internal_error failure
// for stage. A runtime error keeps its message, which the Go runtime builds
// from types and indexes only; any other value is named by its type, since
// it can hold input data. Call it from the deferred function that recovered,
// so the stack still holds the panicking frames.
func PanicFailure(value any, stage string) *Failure {
	if failure, ok := value.(*Failure); ok && failure.Issue.ReasonCode == ReasonPanic {
		// A panic re-raised by an owner goroutine keeps the original stack.
		return failure
	}
	what := fmt.Sprintf("a %T value", value)
	if err, ok := value.(runtime.Error); ok {
		what = err.Error()
	}
	return &Failure{Issue: Issue{
		Code: CodeInternalError, ReasonCode: ReasonPanic, Stage: stage,
		Reason:     "Diffaro panicked: " + what,
		Action:     DefectAction,
		Diagnostic: &Diagnostic{Stack: panicStack()},
	}}
}

// RecoverAs, deferred, turns a panic of the calling goroutine into an
// internal_error failure stored in *err, so the panic fails only the work
// that returns err.
func RecoverAs(err *error, stage string) {
	if value := recover(); value != nil {
		*err = PanicFailure(value, stage)
	}
}

// panicStack returns the frames below the runtime's panic handling, or the
// caller's frames when no panic is in progress.
func panicStack() []string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	var all []runtime.Frame
	panicking := -1
	for {
		frame, more := frames.Next()
		if frame.Function == "runtime.gopanic" {
			panicking = len(all)
		}
		all = append(all, frame)
		if !more {
			break
		}
	}
	// Skip the recovering frames, gopanic and the runtime frames that raised
	// the panic, such as panicmem or goPanicIndex.
	start := panicking + 1
	for panicking >= 0 && start < len(all) && strings.HasPrefix(all[start].Function, "runtime.") {
		start++
	}
	stack := []string{}
	for _, frame := range all[start:] {
		if len(stack) == maxPanicFrames || frame.Function == "runtime.goexit" {
			break
		}
		stack = append(stack, frame.Function+" "+shortFile(frame.File, frame.Line))
	}
	return stack
}
