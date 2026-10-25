package preview

import (
	"fmt"
	"slices"
	"strings"

	"diffaro/internal/domain"
)

// transientFailures may succeed when the same run is repeated.
var transientFailures = map[string]bool{domain.CodeConnectionFailed: true, domain.CodeRateLimited: true, domain.CodeRenderContextChanged: true, domain.CodeSelectionChanged: true, domain.CodeCancelled: true, domain.CodeTimeout: true}

// deterministicFailures repeat for the same inputs: a missing path or
// revision, an invalid chart or input, or a render the backend rejects.
var deterministicFailures = map[string]bool{domain.CodeLocalRenderFailed: true, domain.CodeNotFound: true, domain.CodeRenderFailed: true, domain.CodeRepoServerFailed: true, domain.CodeRevisionInvalid: true, domain.CodeSourceInvalid: true, domain.CodeDestinationInvalid: true, domain.CodeChildSpecInvalid: true}

func transient(issue domain.Issue) bool {
	return transientFailures[issue.Code] || issue.ReasonCode == domain.ReasonTimeout || issue.ReasonCode == domain.ReasonServerError
}

// AnnotateFailureOrigins sets FailureOrigin on Applications with a failed
// base or head target. It only labels outcomes; states, changesFound and the
// exit code are unchanged.
func AnnotateFailureOrigins(report *domain.Report) {
	unchanged := map[domain.ApplicationID]bool{}
	for _, scope := range report.Discovery {
		for _, decision := range scope.Decisions {
			if decision.InputsUnchanged {
				unchanged[decision.ID] = true
			}
		}
	}
	for i := range report.Applications {
		app := &report.Applications[i]
		app.FailureOrigin = failureOrigin(*app, unchanged[app.ID])
	}
}

func failureOrigin(app domain.ApplicationResult, inputsUnchanged bool) domain.FailureOrigin {
	if app.BaseTarget == nil || app.HeadTarget == nil {
		return ""
	}
	base, head := app.BaseTarget.Execution, app.HeadTarget.Execution
	if base != domain.ExecutionFailed && head != domain.ExecutionFailed {
		return ""
	}
	if slices.ContainsFunc(app.Issues, transient) {
		return domain.FailureTransient
	}
	switch {
	case base == domain.ExecutionComplete && head == domain.ExecutionFailed:
		return domain.FailureIntroduced
	case base == domain.ExecutionFailed && head == domain.ExecutionComplete:
		return domain.FailureResolved
	case base != domain.ExecutionFailed || head != domain.ExecutionFailed:
		return ""
	}
	baseCauses, headCauses := causes(app.Issues, "base"), causes(app.Issues, "head")
	if len(baseCauses) == 0 || !slices.Equal(baseCauses, headCauses) {
		return ""
	}
	deterministic := !slices.ContainsFunc(app.Issues, func(issue domain.Issue) bool { return !deterministicFailures[issue.Code] })
	if deterministic && inputsUnchanged && len(app.InputChanges) == 0 {
		return domain.FailurePreexisting
	}
	if len(app.InputChanges) > 0 {
		return domain.FailureInputsChanged
	}
	return domain.FailureBothSides
}

// causes lists one side's distinct failure causes in sorted order; the
// revision differs between sides and is not part of a cause.
func causes(issues []domain.Issue, side string) []string {
	var out []string
	for _, issue := range issues {
		if issue.Side == side {
			out = append(out, strings.Join([]string{issue.Code, issue.ReasonCode, issue.Stage, issue.Reason}, "\x00"))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ExcludePreexistingFailures applies failures.excludePreexistingFromExit:
// Applications whose failure predates the PR no longer make a run with other useful
// results a partial failure. Every other failure keeps the status, a run
// without useful results stays failed, and the excluded Applications stay
// in the report with a notice.
func ExcludePreexistingFailures(report *domain.Report) {
	preexisting := map[domain.ApplicationID]bool{}
	others := false
	for _, app := range report.Applications {
		switch {
		case app.FailureOrigin == domain.FailurePreexisting:
			preexisting[app.ID] = true
		case app.Execution.FailedOrDenied():
			others = true
		}
	}
	if len(preexisting) == 0 {
		return
	}
	// Application issues are also listed at report level.
	for _, issue := range report.Issues {
		if issue.Application == nil || !preexisting[*issue.Application] {
			others = true
		}
	}
	report.PreexistingFailuresExcluded = len(preexisting)
	report.Limitations = append(report.Limitations, fmt.Sprintf(domain.PreexistingExcludedNotice, len(preexisting)))
	if report.Status == domain.StatusPartialFailure && !others {
		report.Status = domain.StatusSuccess
	}
}
