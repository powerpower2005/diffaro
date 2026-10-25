package preview

import (
	"fmt"

	"diffaro/internal/domain"
)

// noteObservedScopes discloses identity inferred from Argo CD's current state.
func noteObservedScopes(report *domain.Report) {
	for _, app := range report.Applications {
		for _, resource := range app.Resources {
			if resource.ScopeSource != "" {
				report.Limitations = append(report.Limitations, "Some resource namespaces were inferred from the resource scope Argo CD currently reports for the Application or, in repository discovery, for other Applications in the same destination cluster (scopeSource: argocd-observed). The same scope applies to base and head; it is current-state metadata, not target evidence.")
				return
			}
		}
	}
}

// noteIgnoreRules states that ignore rules were not applied when any compared
// Application declares them (only those without RespectIgnoreDifferences are
// compared; see domain.Application.IgnoreRulesAffectTargets).
func noteIgnoreRules(report *domain.Report) {
	count := 0
	for _, app := range report.Applications {
		declared := app.Inputs.IgnoreDifferences > 0
		for _, side := range []*domain.TargetSummary{app.BaseTarget, app.HeadTarget} {
			declared = declared || (side != nil && side.Inputs.IgnoreDifferences > 0)
		}
		if declared {
			count++
		}
	}
	if count > 0 {
		report.Limitations = append(report.Limitations, fmt.Sprintf("%d compared Application(s) declare ignoreDifferences without RespectIgnoreDifferences (inputs.ignoreDifferences). Those rules affect only Argo CD live drift status, not what sync applies, so they were not applied: changes to ignored fields are reported.", count))
	}
}

// ApplyRunIssues records issues found outside per-Application comparison
// (generated selection, repository discovery, backend setup). It degrades the
// status to failed, partial_failure or denied and marks graph discovery
// incomplete.
func ApplyRunIssues(report *domain.Report, issues []domain.Issue) {
	if len(issues) == 0 {
		return
	}
	report.Issues = append(report.Issues, issues...)
	if report.Status != domain.StatusCancelled && report.Status != domain.StatusInvalidConfiguration {
		aggregateStatus(report, false, report.Status == domain.StatusPartialFailure)
	}
	if report.Graph != nil {
		report.Graph.BaseComplete, report.Graph.HeadComplete = false, false
	}
}

// finishReport derives what a run reports as a whole from its
// Applications, run issues and graph edge changes: whether changes were
// found, then the aggregate status. Every mode and Doctor end with it.
func finishReport(report *domain.Report, cancelled bool) {
	if report.Graph != nil {
		for _, edge := range report.Graph.Changes {
			if edge.State == domain.StateAdded || edge.State == domain.StateRemoved {
				report.ChangesFound = true
			}
		}
	}
	for _, app := range report.Applications {
		if app.State.IsChange() {
			report.ChangesFound = true
		}
		for _, resource := range app.Resources {
			if resource.State.IsChange() {
				report.ChangesFound = true
			}
		}
	}
	aggregateStatus(report, cancelled, false)
}

// aggregateStatus sets the report status (domain.Report.SetAggregateStatus)
// from every failed or denied Application and every run issue. A useful
// Application result (usefulResult), or partial, makes failures a partial
// failure.
func aggregateStatus(report *domain.Report, cancelled, partial bool) {
	failures, denied, useful := 0, 0, partial
	for _, app := range report.Applications {
		if app.Execution.FailedOrDenied() {
			failures++
			if app.Execution == domain.ExecutionDenied {
				denied++
			}
		}
		useful = useful || usefulResult(app)
	}
	for _, issue := range report.Issues {
		failures++
		if issue.Code == domain.CodeAccessDenied {
			denied++
		}
	}
	report.SetAggregateStatus(cancelled, failures, denied, useful)
}
