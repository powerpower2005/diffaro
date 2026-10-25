package domain

// Status is the overall outcome of a run (Report.Status). ExitCode maps each
// value to the documented exit code.
type Status string

const (
	StatusSuccess              Status = "success"
	StatusPolicyViolation      Status = "policy_violation"
	StatusPartialFailure       Status = "partial_failure"
	StatusInvalidConfiguration Status = "invalid_configuration"
	StatusDenied               Status = "denied"
	StatusFailed               Status = "failed"
	StatusCancelled            Status = "cancelled"
)

// Process exit codes (docs/reference.md). Commands without a report, such as
// init and invalid arguments, use the same codes.
const (
	ExitSuccess              = 0
	ExitChangesFound         = 1
	ExitPolicyViolation      = 2
	ExitPartialFailure       = 3
	ExitInvalidConfiguration = 4
	ExitDenied               = 5
	ExitFailed               = 6
	ExitCancelled            = 7
)

// State is what a comparison observed for an Application, a resource, a
// render input or a graph edge. Resources, inputs and edges use added,
// removed, changed, unchanged and unknown; an Application result can also
// be failed, denied or skipped when it could not be compared.
type State string

const (
	StateAdded     State = "added"
	StateRemoved   State = "removed"
	StateChanged   State = "changed"
	StateUnchanged State = "unchanged"
	StateUnknown   State = "unknown"
	StateFailed    State = "failed"
	StateDenied    State = "denied"
	StateSkipped   State = "skipped"
)

// Execution is how far the analysis of an Application or one of its
// targets got.
type Execution string

const (
	ExecutionComplete Execution = "complete"
	ExecutionFailed   Execution = "failed"
	ExecutionDenied   Execution = "denied"
	ExecutionSkipped  Execution = "skipped"
)

// Comparison is how completely a resource or render input was compared.
// path-only applies to render inputs whose content is not compared.
type Comparison string

const (
	ComparisonComplete   Comparison = "complete"
	ComparisonIncomplete Comparison = "incomplete"
	ComparisonPathOnly   Comparison = "path-only"
)

// Operation is the kind of a field change.
type Operation string

const (
	OperationAdd     Operation = "add"
	OperationRemove  Operation = "remove"
	OperationReplace Operation = "replace"
)

// FailureOrigin compares a failed Application's base and head outcomes. It labels outcomes only; states, changesFound and the exit code
// do not depend on it, except through failures.excludePreexistingFromExit.
type FailureOrigin string

const (
	// FailureIntroduced: base renders and head fails.
	FailureIntroduced FailureOrigin = "introduced"
	// FailurePreexisting: base and head fail with the same deterministic
	// cause and the PR changes none of the Application's inputs.
	FailurePreexisting FailureOrigin = "preexisting"
	// FailureInputsChanged: base and head fail with the same cause and the
	// PR changes inputs the Application reads.
	FailureInputsChanged FailureOrigin = "inputs-changed"
	// FailureBothSides: base and head fail with the same cause, and whether
	// the PR changes the Application's inputs cannot be determined.
	FailureBothSides FailureOrigin = "both-sides"
	// FailureResolved: base fails and head renders.
	FailureResolved FailureOrigin = "resolved"
	// FailureTransient: a failure that may succeed on retry.
	FailureTransient FailureOrigin = "transient"
)

// IsChange reports an added, changed or removed state.
func (s State) IsChange() bool {
	return s == StateAdded || s == StateChanged || s == StateRemoved
}

// FailedOrDenied reports an execution that failed, including denied access.
func (e Execution) FailedOrDenied() bool {
	return e == ExecutionFailed || e == ExecutionDenied
}
