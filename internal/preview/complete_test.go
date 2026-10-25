package preview

import (
	"context"
	"slices"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Every comparison ends with the same steps in a fixed order: the
// changed-path notice after the run's own limitations, repeated values
// masked, the inputChanges boundary, the PR file list failure first, failure
// origins, then backend notes, read after every other step.
func TestCompleteComparisonAppliesPostRunStepsInOrder(t *testing.T) {
	masked := &security.MaskedValues{}
	masked.Add("fictional-shared-key")
	report := domain.Report{Limitations: []string{"run limitation"}, Applications: []domain.ApplicationResult{{
		ID:         domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "api"},
		BaseTarget: &domain.TargetSummary{Execution: domain.ExecutionComplete},
		HeadTarget: &domain.TargetSummary{Execution: domain.ExecutionFailed},
		Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Kind: "ConfigMap", Name: "copy"}, Changes: []domain.Change{
			{Path: "/data/copy", Operation: domain.OperationReplace, Before: "visible", After: "fictional-shared-key"},
		}}},
	}}}
	var limitationsAtNotes []string
	evidence := ComparisonEvidence{SelectionNotice: "selection notice", FileListFailure: "file list failure", Notes: func() []string {
		limitationsAtNotes = slices.Clone(report.Limitations)
		return []string{"backend note"}
	}}
	Runner{Redactor: security.Redactor{Masked: masked}}.CompleteComparison(context.Background(), &report, evidence)
	want := []string{"file list failure", "run limitation", "selection notice", inputFilesUnavailable, "backend note"}
	if !slices.Equal(report.Limitations, want) {
		t.Fatalf("limitations: %q, want %q", report.Limitations, want)
	}
	if !slices.Equal(limitationsAtNotes, want[:len(want)-1]) {
		t.Fatalf("notes were read before the other steps: %q", limitationsAtNotes)
	}
	app := report.Applications[0]
	if app.FailureOrigin != domain.FailureIntroduced {
		t.Fatalf("failure origin: %q", app.FailureOrigin)
	}
	if c := app.Resources[0].Changes[0]; c.After != security.Mask || !c.Redacted {
		t.Fatalf("repeated value not masked: %+v", c)
	}
}
