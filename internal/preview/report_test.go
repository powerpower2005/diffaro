package preview

import (
	"testing"

	"diffaro/internal/domain"
)

// finishReport counts every failed Application and every run issue: a run
// issue alone fails the run, denied needs every failure to be a denial, and a
// useful result keeps failures a partial failure.
func TestFinishReportCountsApplicationsAndRunIssues(t *testing.T) {
	app := func(execution domain.Execution) domain.ApplicationResult {
		return domain.ApplicationResult{State: domain.StateUnknown, Execution: execution}
	}
	denied := domain.Issue{Code: domain.CodeAccessDenied}
	failed := domain.Issue{Code: domain.CodeRenderFailed}
	for _, tc := range []struct {
		name   string
		apps   []domain.ApplicationResult
		issues []domain.Issue
		want   domain.Status
	}{
		{"run issue without Applications", nil, []domain.Issue{failed}, domain.StatusFailed},
		{"denied Application and denied run issue", []domain.ApplicationResult{app(domain.ExecutionDenied)}, []domain.Issue{denied}, domain.StatusDenied},
		{"denied Application and failed run issue", []domain.ApplicationResult{app(domain.ExecutionDenied)}, []domain.Issue{failed}, domain.StatusFailed},
		{"complete Application and run issue", []domain.ApplicationResult{app(domain.ExecutionComplete)}, []domain.Issue{failed}, domain.StatusPartialFailure},
		{"skipped Application only", []domain.ApplicationResult{app(domain.ExecutionSkipped)}, nil, domain.StatusSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.NewReport()
			report.Applications, report.Issues = append(report.Applications, tc.apps...), append(report.Issues, tc.issues...)
			finishReport(&report, false)
			if report.Status != tc.want {
				t.Fatalf("status %s, want %s", report.Status, tc.want)
			}
		})
	}
}

// Changes are found from an added or removed graph edge, a changed
// Application or a changed resource, in every mode.
func TestFinishReportFindsChangesFromEdgesApplicationsAndResources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report func(*domain.Report)
		want   bool
	}{
		{"added edge", func(r *domain.Report) {
			r.Graph = &domain.GraphSummary{Changes: []domain.GraphEdgeChange{{State: domain.StateAdded}}}
		}, true},
		{"removed Application", func(r *domain.Report) {
			r.Applications = append(r.Applications, domain.ApplicationResult{State: domain.StateRemoved, Execution: domain.ExecutionComplete})
		}, true},
		{"changed resource", func(r *domain.Report) {
			r.Applications = append(r.Applications, domain.ApplicationResult{State: domain.StateUnknown, Execution: domain.ExecutionFailed, Resources: []domain.ResourceDiff{{State: domain.StateChanged}}})
		}, true},
		{"unknown resource", func(r *domain.Report) {
			r.Applications = append(r.Applications, domain.ApplicationResult{State: domain.StateUnknown, Execution: domain.ExecutionComplete, Resources: []domain.ResourceDiff{{State: domain.StateUnknown}}})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.NewReport()
			tc.report(&report)
			finishReport(&report, false)
			if report.ChangesFound != tc.want {
				t.Fatalf("changes found %v, want %v", report.ChangesFound, tc.want)
			}
		})
	}
}
