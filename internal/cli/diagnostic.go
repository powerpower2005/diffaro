package cli

import (
	"errors"
	"fmt"
	"io"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

func printEncodingFailure(out io.Writer, err error, redactor security.Redactor) {
	var failure *domain.Failure
	if errors.As(err, &failure) {
		printIssue(out, failure.Issue, redactor)
		return
	}
	fmt.Fprintln(out, "cannot encode bounded report; reduce scope and check output format")
}

// printPanic reports a panic that no comparison boundary caught, with the
// build identity a bug report needs.
func printPanic(out io.Writer, failure *domain.Failure) {
	fmt.Fprint(out, "Diffaro internal error. ")
	printVersion(out)
	printIssue(out, failure.Issue, security.Redactor{})
}

func printIssue(out io.Writer, issue domain.Issue, redactor security.Redactor) {
	reporter.WriteIssue(out, issue, redactor)
}

func printDeniedSummary(out io.Writer) {
	reporter.WriteDeniedSummary(out)
}

// printReportDiagnostics writes a report's diagnostics to stderr within the
// console byte budget.
func printReportDiagnostics(out io.Writer, report domain.Report, redactor security.Redactor, verbose bool) {
	bounded := &diagnosticOutput{writer: out, remaining: 1 << 20}
	defer bounded.finish()
	reporter.WriteDiagnostics(bounded, report, redactor, verbose)
}

// Console diagnostics have independent byte and record budgets. Full evidence
// remains in JSON, including records omitted from a large console log.
type diagnosticOutput struct {
	writer    io.Writer
	remaining int
	truncated bool
}

func (d *diagnosticOutput) Write(p []byte) (int, error) {
	if d.truncated || len(p) > d.remaining {
		d.truncated = true
		return len(p), nil
	}
	n, err := d.writer.Write(p)
	d.remaining -= n
	return n, err
}

func (d *diagnosticOutput) finish() {
	if d.truncated {
		fmt.Fprintln(d.writer, "Diagnostic byte limit reached; inspect the full JSON report for remaining evidence.")
	}
}
