package preview

import (
	"context"
	"fmt"
	"sort"

	"diffaro/internal/domain"
)

// discoveryFailedReason is the decision reason of a repository candidate
// that is not rendered because its instance's discovery failed.
const discoveryFailedReason = "same-source-repository-discovery-failed"

const discoveryBoundary = domain.LimitationDiscoveryBoundary

// RunDiscovered compares repository candidates. Compare also attaches input
// evidence while reusing this discovery's head chart readings.
func (r Runner) RunDiscovered(ctx context.Context, pair domain.RevisionPair, inventories map[string]domain.ApplicationInventory) domain.Report {
	report, _ := r.runDiscovered(ctx, pair, inventories)
	return report
}

// runDiscovered returns the head chart readings to this comparison's input
// analysis. They never become part of the Runner configuration.
func (r Runner) runDiscovered(ctx context.Context, pair domain.RevisionPair, inventories map[string]domain.ApplicationInventory) (domain.Report, *changedPathSelector) {
	candidates := []domain.Application{}
	scopes := clusterScopes{}
	snapshots := []discoverySnapshot{}
	results := []domain.DiscoveryResult{}
	issues := []domain.Issue{}
	matched, unaffected := 0, 0
	manages := map[domain.ApplicationID]bool{}
	otherHosts := map[string]int{}
	var selector *changedPathSelector
	if r.ChangedPaths != nil {
		selector = newChangedPathSelector(*r.ChangedPaths, r.Config.Git.Matches)
	}
	instances := append([]string{}, instanceIDs(r)...)
	sort.Strings(instances)
	for _, instance := range instances {
		scope := discoveryScope{Instance: instance, Projects: r.Config.Discovery.Projects}
		stage, cancel := context.WithTimeout(ctx, r.Config.Timeout())
		snapshot, err := discoverRepository(stage, scope, r.Config.Git.RepositoryURL, inventories[instance], r.Config.Render.MaxApplications, r.Config.Git.Aliases()...)
		cancel()
		result := domain.DiscoveryResult{Instance: instance, Projects: append([]string{}, scope.Projects...), Status: "complete", Decisions: snapshot.Decisions}
		if err == nil && len(candidates)+len(snapshot.Applications) > r.Config.Render.MaxApplications {
			err = domain.LimitError(domain.CodeSelectionLimitExceeded, "selection", "combined repository candidates across Argo CD instances", "render.maxApplications", int64(len(candidates)+len(snapshot.Applications)), int64(r.Config.Render.MaxApplications), "Applications", false, domain.MaxRenderApplications)
		}
		if err != nil {
			result.Status = "failed"
			// None of the instance's candidates is rendered.
			for i := range result.Decisions {
				if result.Decisions[i].Selected {
					result.Decisions[i].Selected, result.Decisions[i].Reason = false, discoveryFailedReason
				}
			}
			issues = append(issues, ContextualIssue(err, domain.ApplicationID{Instance: instance}, r.Config.Git.RepositoryURL, ""))
		} else {
			if len(snapshot.Applications) == 0 {
				result.Status = "no_matches"
			}
			matched += len(snapshot.Applications)
			selected := snapshot.Applications
			if selector != nil {
				selected = r.selectChangedPaths(ctx, selector, snapshot, &result)
				unaffected += result.Unaffected
			}
			candidates = append(candidates, selected...)
			snapshots = append(snapshots, snapshot)
			for id, value := range snapshot.ManagesApplications {
				manages[id] = value
			}
			for key, kinds := range snapshot.Scopes {
				scopes[key] = kinds
			}
			for source, n := range snapshot.OtherHosts {
				otherHosts[source] += n
			}
		}
		results = append(results, result)
	}
	var descendants *descendantSelector
	if selector != nil {
		descendants = newDescendantSelector(selector, pair, manages, candidates)
	}
	report := r.newGraphRun(pair, scopes, descendants).compareGraphs(ctx, candidates)
	report.Discovery = results
	report.Limitations = append([]string{discoveryBoundary}, report.Limitations...)
	if selector != nil {
		skipped := descendants.annotate(report.Discovery)
		if unaffected == 0 && skipped == 0 {
			report.Limitations = append(report.Limitations, fmt.Sprintf("Changed-path selection rendered all %d repository candidates and their descendants: none was proven unaffected by the PR files. Reasons are in discovery decisions.", matched))
		} else {
			report.Limitations = append(report.Limitations, fmt.Sprintf("Changed-path selection did not render %d of %d repository candidates and %d descendants: no PR file is under their source path, value files or local chart dependencies. Base and head use the same stored spec and backend, so their targets are identical; a descendant is skipped only when its parent renders it with the same spec on both revisions and Argo CD status shows it manages no child Applications. A candidate whose source path exists at neither revision is not rendered either. Reasons are in discovery decisions.", unaffected, matched, skipped))
		}
	}
	if matched == 0 && len(issues) == 0 {
		report.Limitations = append(report.Limitations, "No readable Application matched the repository within the selected projects; this is not a successful comparison of application targets.")
	}
	if hint := OtherHostHint(otherHosts); hint != "" {
		// First, so the compact PR Markdown shows the missing setting.
		report.Limitations = append([]string{hint}, report.Limitations...)
	}
	for _, snapshot := range snapshots {
		stage, cancel := context.WithTimeout(ctx, r.Config.Timeout())
		err := verifyDiscovery(stage, snapshot, r.Config.Git.RepositoryURL, inventories[snapshot.Scope.Instance], r.Config.Render.MaxApplications, r.Config.Git.Aliases()...)
		cancel()
		if err != nil {
			issues = append(issues, ContextualIssue(err, domain.ApplicationID{Instance: snapshot.Scope.Instance}, r.Config.Git.RepositoryURL, ""))
			for i := range report.Discovery {
				if report.Discovery[i].Instance == snapshot.Scope.Instance {
					report.Discovery[i].Status = "failed"
				}
			}
		}
	}
	ApplyRunIssues(&report, issues)
	return report, selector
}

func instanceIDs(r Runner) []string {
	ids := []string{}
	for _, instance := range r.Config.Instances {
		ids = append(ids, instance.ID)
	}
	return ids
}

// selectChangedPaths records a selection for every candidate and returns the
// candidates that still need rendering. Failures to prove independence render.
func (r Runner) selectChangedPaths(ctx context.Context, selector *changedPathSelector, snapshot discoverySnapshot, result *domain.DiscoveryResult) []domain.Application {
	index := map[domain.ApplicationID]int{}
	for i, decision := range result.Decisions {
		index[decision.ID] = i
	}
	type decided struct {
		selection, reason string
		unchanged         bool
	}
	// Candidates read concurrently; indexed results keep the order.
	decisions := make([]decided, len(snapshot.Applications))
	bounded(len(snapshot.Applications), r.Config.Render.MaxConcurrency, func(i int) {
		app := snapshot.Applications[i]
		var manages *bool
		if value, ok := snapshot.ManagesApplications[app.ID]; ok {
			manages = &value
		}
		// Each candidate reads within its own stage timeout, so one slow
		// repository does not exhaust the budget of later candidates.
		stage, cancel := context.WithTimeout(ctx, r.Config.Timeout())
		defer cancel()
		d := &decisions[i]
		d.selection, d.reason = selector.decide(stage, app, manages)
		d.unchanged = d.selection == selectionUnaffected || d.selection == selectionSourceMissing || (d.selection == selectionIneligible && selector.inputsUnchanged(stage, app))
	})
	rendered := []domain.Application{}
	for k, app := range snapshot.Applications {
		selection, reason, unchanged := decisions[k].selection, decisions[k].reason, decisions[k].unchanged
		if i, ok := index[app.ID]; ok {
			result.Decisions[i].Selection, result.Decisions[i].SelectionReason = selection, reason
			result.Decisions[i].InputsUnchanged = unchanged
		}
		if selection == selectionUnaffected || selection == selectionSourceMissing {
			result.Unaffected++
			continue
		}
		rendered = append(rendered, app)
	}
	return rendered
}
