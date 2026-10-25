package domain

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"runtime"
	"syscall"
)

// Diagnostic holds safe, structured detail that classifies a failure
// without its raw text. It never holds values, file
// contents, URLs or error messages.
type Diagnostic struct {
	// Stack lists the panicking goroutine's frames, innermost first, as
	// "function file:line" with the file's directory and name only.
	Stack []string `json:"stack,omitempty"`
	// Site is the Diffaro source line that created the failure, such as
	// "httpx/client.go:117".
	Site string `json:"site,omitempty"`
	// Chain names the wrapped cause, outermost first: Go type names, the
	// names of standard sentinel errors and operating system error numbers,
	// such as "*net.OpError" or "syscall.Errno(connection reset by peer)".
	Chain []string `json:"chain,omitempty"`
	// HTTPStatus is the status of the provider response behind the failure.
	HTTPStatus int `json:"httpStatus,omitempty"`
	// Route is the fixed route template of the provider request, never a URL.
	Route string `json:"route,omitempty"`
	// ContentType is the media type of a provider response that could not
	// be decoded, such as "text/html", without parameters.
	ContentType string `json:"contentType,omitempty"`
}

// maxChainLinks bounds a cause chain.
const maxChainLinks = 12

// sentinels names standard errors whose type alone says nothing.
var sentinels = []struct {
	err  error
	name string
}{
	{io.EOF, "io.EOF"}, {io.ErrUnexpectedEOF, "io.ErrUnexpectedEOF"}, {io.ErrShortWrite, "io.ErrShortWrite"}, {io.ErrClosedPipe, "io.ErrClosedPipe"},
	{context.Canceled, "context.Canceled"}, {context.DeadlineExceeded, "context.DeadlineExceeded"},
	{fs.ErrNotExist, "fs.ErrNotExist"}, {fs.ErrPermission, "fs.ErrPermission"}, {fs.ErrExist, "fs.ErrExist"}, {fs.ErrClosed, "fs.ErrClosed"},
}

// ErrorChain names err and its wrapped causes without their messages. An
// operating system error number keeps its fixed system description, which
// holds no input.
func ErrorChain(err error) []string {
	var chain []string
	queue := []error{err}
	for len(queue) > 0 && len(chain) < maxChainLinks {
		current := queue[0]
		queue = queue[1:]
		if current == nil {
			continue
		}
		chain = append(chain, chainLink(current))
		switch wrapped := current.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, wrapped.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, wrapped.Unwrap()...)
		}
	}
	return chain
}

func chainLink(err error) string {
	for _, sentinel := range sentinels {
		if err == sentinel.err {
			return sentinel.name
		}
	}
	switch value := err.(type) {
	case syscall.Errno:
		return "syscall.Errno(" + value.Error() + ")"
	case *Failure:
		return "*domain.Failure(" + value.Issue.Code + ")"
	}
	return fmt.Sprintf("%T", err)
}

// callSite is the source line skip frames above its caller, as
// "directory/file.go:line".
func callSite(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return ""
	}
	return shortFile(file, line)
}

func shortFile(file string, line int) string {
	return fmt.Sprintf("%s:%d", path.Join(path.Base(path.Dir(file)), path.Base(file)), line)
}

// located returns a diagnostic for a failure created skip frames above its
// caller with cause.
func located(skip int, cause error) *Diagnostic {
	d := &Diagnostic{Site: callSite(skip + 1)}
	if cause != nil {
		d.Chain = ErrorChain(cause)
	}
	return d
}

// Locate is Fail's diagnostic for failures built as literals: the caller's
// source line and the chain of cause.
func Locate(cause error) *Diagnostic { return located(1, cause) }

// UntypedDiagnostic classifies an error that carries no Failure, such as one
// returned unwrapped by a library, by its cause chain only.
func UntypedDiagnostic(err error) *Diagnostic {
	if err == nil {
		return nil
	}
	return &Diagnostic{Chain: ErrorChain(err)}
}

// With returns a copy of d changed by edit, so failures shared by
// independent branches are never mutated.
func (d *Diagnostic) With(edit func(*Diagnostic)) *Diagnostic {
	copy := Diagnostic{}
	if d != nil {
		copy = *d
	}
	edit(&copy)
	return &copy
}

// DefectAction is the action of an internal_error: it is a Diffaro defect.
const DefectAction = "this is a Diffaro bug; rerun with --debug-bundle FILE --debug-bundle-level public and attach the bundle to a bug report, or report the output of diffaro --version and this issue's diagnostic"

// UnclassifiedIssue is the internal_error for an error that carries no
// Failure: no adapter classified it, so it is a Diffaro defect. Its
// diagnostic keeps the cause chain without text.
func UnclassifiedIssue(err error, stage, reason string) Issue {
	return Issue{Code: CodeInternalError, ReasonCode: ReasonUnclassified, Stage: stage, Reason: reason, Action: DefectAction, Diagnostic: UntypedDiagnostic(err)}
}
