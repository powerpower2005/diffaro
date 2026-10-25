package domain

// RenderSkipped identifies an intentional analysis boundary, not a render
// failure. Adapters must never use it for denied access or backend failures.
type RenderSkipped struct {
	Reason string
	Cause  error
}

func (e *RenderSkipped) Error() string { return e.Reason }
func (e *RenderSkipped) Unwrap() error { return e.Cause }

const DetailedChildAnalysisSkipReason = "Basic analysis skips resource rendering and descendant discovery for a new or reconfigured child. For optional detailed analysis, select local Helm (--local-helm or --helm-binary) for supported Git Helm children, or configure a trusted repo-server backend."
