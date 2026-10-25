package preview

import (
	"context"
	"errors"
	"strings"

	"diffaro/internal/domain"
)

// ContextualIssue converts an error into a domain.Issue for one Application.
// Cancellation and timeouts get their own codes and keep the reason and stage
// of the step they stopped when the error names one, such as a request or a
// render; Finalize adds the configured render.timeout. Repository
// and revision are set unless the issue already names a specific source
// position.
func ContextualIssue(err error, id domain.ApplicationID, repository, revision string) domain.Issue {
	i := domain.UnclassifiedIssue(err, "preview", "preview could not complete")
	var f *domain.Failure
	// A step keeps its reason even when it already carries the stop code,
	// such as a local Helm render its context stopped; only the generic
	// cancellation names no step.
	step := errors.As(err, &f) && f.Issue.Reason != cancelledReason
	if f != nil {
		i = f.Issue
	}
	// The cause's own action, such as checking connectivity for a request
	// the cancellation interrupted, does not apply.
	if errors.Is(err, context.Canceled) {
		i.Code, i.ReasonCode = domain.CodeCancelled, ""
		i.Reason = cancelledReason
		if step {
			i.Reason = f.Issue.Reason + "; the run was cancelled before this step finished"
		}
		i.Action = "rerun the preview; the run was cancelled before this step finished"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		i.Code, i.ReasonCode = domain.CodeTimeout, ""
		i.Reason = "a preview step " + timeoutLimit
		if step {
			i.Reason = f.Issue.Reason + "; " + timeoutLimit
		}
		i.Action = "rerun the preview, or raise render.timeout if this step needs longer"
	}
	i.Application = &id
	if i.SourcePosition == 0 {
		i.Repository = repository
		i.Revision = revision
	}
	return i
}

// cancelledReason is the reason of a cancellation that names no step.
const cancelledReason = "preview was cancelled"

// timeoutLimit ends the reason of a timeout issue; nameTimeoutLimit adds the
// configured value.
const timeoutLimit = "exceeded render.timeout"

// nameTimeoutLimit adds the configured render.timeout to the reason of every
// timeout issue ContextualIssue described, such as "exceeded render.timeout
// (5s)".
func nameTimeoutLimit(report *domain.Report, limit string) {
	name := func(issues []domain.Issue) {
		for i := range issues {
			if issues[i].Code == domain.CodeTimeout && strings.HasSuffix(issues[i].Reason, timeoutLimit) {
				issues[i].Reason += " (" + limit + ")"
			}
		}
	}
	name(report.Issues)
	for i := range report.Applications {
		name(report.Applications[i].Issues)
	}
}

// issueCode is the code ContextualIssue gives err.
func issueCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return domain.CodeTimeout
	case errors.Is(err, context.Canceled):
		return domain.CodeCancelled
	}
	var f *domain.Failure
	if errors.As(err, &f) {
		return f.Issue.Code
	}
	return domain.CodeInternalError
}

// executionFor is the execution of an Application whose sides failed with
// errs: denied only when every failing side was denied access, otherwise
// failed, whatever the order of the sides.
func executionFor(errs ...error) domain.Execution {
	for _, err := range errs {
		if issueCode(err) != domain.CodeAccessDenied {
			return domain.ExecutionFailed
		}
	}
	return domain.ExecutionDenied
}

// failedSide is one side of an Application and its error, nil when the side
// did not fail.
type failedSide struct {
	name, repository, revision string
	err                        error
}

// markSideFailures records the issues of every failing side on out, sets
// the execution those sides decide (executionFor) and leaves an unchanged
// Application unknown. It returns the failing sides' errors.
func markSideFailures(out *domain.ApplicationResult, sides ...failedSide) []error {
	var errs []error
	for _, side := range sides {
		if side.err == nil {
			continue
		}
		out.Issues = append(out.Issues, sideIssues(side.err, out.ID, side.repository, side.revision, side.name)...)
		errs = append(errs, side.err)
	}
	if len(errs) > 0 {
		out.Execution = executionFor(errs...)
		if out.State == domain.StateUnchanged {
			out.State = domain.StateUnknown
		}
	}
	return errs
}
