package preview

import (
	"encoding/json"

	"diffaro/internal/domain"
)

// retainBudget bounds the encoded bytes of Application results kept in the
// report. Results are offered in report order; once one does not fit, no
// later one is kept, so the retained prefix is deterministic.
type retainBudget struct {
	limit, used int
	// observed is the size that first exceeded the limit, for the issue.
	observed  int
	exhausted bool
}

func newRetainBudget(limit int) *retainBudget {
	if limit <= 0 {
		limit = domain.RetainedByteLimit
	}
	return &retainBudget{limit: limit}
}

// keep reports whether result fits in the remaining budget and counts it.
func (b *retainBudget) keep(result domain.ApplicationResult) bool {
	if b.exhausted {
		return false
	}
	encoded, err := json.Marshal(result)
	b.observed = b.used + len(encoded)
	if err == nil && b.observed <= b.limit {
		b.used = b.observed
		return true
	}
	b.exhausted = true
	return false
}

// limitIssue is the issue of a result the budget did not keep.
func (b *retainBudget) limitIssue(subject string, id domain.ApplicationID, repository string) domain.Issue {
	return ContextualIssue(domain.LimitError(domain.CodeReportLimitExceeded, "report", subject, "comparison retained-byte budget", int64(b.observed), int64(b.limit), "bytes", true, 0), id, repository, "")
}

// usefulResult reports whether an Application result keeps a run with
// failures a partial failure instead of a failure (docs/reference.md, "Exit codes"): it
// was compared completely, it was skipped with a stated reason (a skipped
// entry is a successful outcome, as in a run that exits 0), or it kept a
// resource diff. Every mode and ApplyRunIssues use this one definition.
func usefulResult(app domain.ApplicationResult) bool {
	return app.Execution == domain.ExecutionComplete || app.Execution == domain.ExecutionSkipped || len(app.Resources) > 0
}
