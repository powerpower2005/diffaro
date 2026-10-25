package domain

import "errors"

// SourceFailure adds known source context without modifying a shared error.
func SourceFailure(err error, source SourceRevision) error {
	if err == nil || source.Position <= 0 {
		return err
	}
	issue := Issue{Code: CodeSourceFailed, Stage: "render", Reason: "source operation could not complete", Action: "check this source's revision, render inputs and repository access"}
	var failure *Failure
	if errors.As(err, &failure) {
		issue = failure.Issue
	} else {
		issue.Diagnostic = UntypedDiagnostic(err)
	}
	issue.SourcePosition, issue.Repository, issue.Revision = source.Position, source.Repository, source.Revision
	return &Failure{Issue: issue, Cause: err}
}
