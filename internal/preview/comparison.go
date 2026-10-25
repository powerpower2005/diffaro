package preview

import (
	"context"

	"diffaro/internal/domain"
)

// Compare runs the selected comparison and completes its evidence in one
// scope. Discovery's head chart cache is passed directly to input analysis,
// so each comparison reuses only its own head chart readings.
// Finalize applies command-specific policies and backend evidence afterward.
func (r Runner) Compare(ctx context.Context, pair domain.RevisionPair, recursive bool, inventories map[string]domain.ApplicationInventory, evidence ComparisonEvidence) domain.Report {
	var report domain.Report
	var head *changedPathSelector
	switch {
	case r.Config.Discovery != nil:
		report, head = r.runDiscovered(ctx, pair, inventories)
	case recursive:
		report = r.RunRecursive(ctx, pair)
	default:
		report = r.Run(ctx, pair)
	}
	r.completeComparison(ctx, &report, evidence, head)
	return report
}
