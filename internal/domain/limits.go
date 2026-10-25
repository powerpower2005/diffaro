package domain

import "fmt"

// RetainedByteLimit bounds each inventory, revision graph and comparison result
// independently. It does not remove per-request or concurrency limits.
const RetainedByteLimit = 64 << 20

// Supported ranges of the profile render limits (render.*). Profile
// validation and the maximum passed to LimitError use the same values.
const (
	MaxRenderDepth               = 64        // render.maxDepth
	MaxRenderConcurrency         = 32        // render.maxConcurrency
	MaxRenderApplications        = 10000     // render.maxApplications
	MinRenderResponseBytes       = 1024      // render.maxResponseBytes
	MaxRenderResponseBytes       = 64 << 20  // render.maxResponseBytes
	MaxRenderDocuments           = 10000     // render.maxDocuments
	MaxRenderSourceSnapshotBytes = 512 << 20 // render.maxSourceSnapshotBytes
)

// LimitError reports measurements, never payloads. A lower-bound observation
// means input collection stopped at the limit and the full size is unknown.
func LimitError(code, stage, subject, setting string, observed, limit int64, unit string, lowerBound bool, maximum int64) error {
	qualifier := ""
	if lowerBound {
		qualifier = "at least "
	}
	reason := fmt.Sprintf("%s exceeds %s: observed %s%d %s; limit %d %s", subject, setting, qualifier, observed, unit, limit, unit)
	action := "narrow the analysis scope and rerun; this retained-output limit is fixed"
	if maximum > 0 {
		if limit < maximum {
			action = fmt.Sprintf("increase %s above %d (supported maximum %d %s), or narrow the analysis scope", setting, limit, maximum, unit)
		} else {
			action = fmt.Sprintf("%s is already at the supported maximum %d %s; narrow the analysis scope", setting, maximum, unit)
		}
	}
	if lowerBound {
		action += "; input collection stopped at the limit, so its full size is unknown"
	}
	// The site is the caller that hit the limit, not this constructor.
	return &Failure{Issue: Issue{Code: code, Stage: stage, Reason: reason, Action: action, Diagnostic: located(1, nil)}}
}
