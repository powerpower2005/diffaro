package preview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

const GeneratedSelectionBoundary = domain.LimitationGeneratedSelection

type SelectionSnapshot struct {
	Source domain.ApplicationID
	Digest [32]byte
}

func generated(ctx context.Context, source domain.ApplicationID, inventory domain.ApplicationInventory, limit int) ([]domain.OwnedApplication, error) {
	if inventory == nil {
		return nil, domain.Fail(domain.CodeSelectionUnavailable, "selection", "Application inventory is unavailable for this instance", "configure a readable Argo CD instance", nil)
	}
	apps, err := inventory.ListApplications(ctx, source.Namespace, limit)
	if err != nil {
		return nil, err
	}
	selected := []domain.OwnedApplication{}
	uid := ""
	for _, app := range apps {
		if app.OwnerName != source.Name {
			continue
		}
		if app.Application.ID.Instance != source.Instance || app.Application.ID.Namespace != source.Namespace || app.OwnerUID == "" || (uid != "" && uid != app.OwnerUID) {
			return nil, domain.Fail(domain.CodeSelectionAmbiguous, "selection", "generated Applications disagree on controller identity", "wait for ApplicationSet reconciliation and check instance/namespace ownership", nil)
		}
		uid = app.OwnerUID
		selected = append(selected, app)
	}
	if len(selected) == 0 {
		return nil, domain.Fail(domain.CodeSelectionEmpty, "selection", "no readable generated Applications match this ApplicationSet", "check namespace/name, Application read permissions and controller reconciliation", nil)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Application.ID.String() < selected[j].Application.ID.String() })
	return selected, nil
}

func SelectGenerated(ctx context.Context, c profile.Config, inventories map[string]domain.ApplicationInventory) (profile.Config, []domain.ApplicationSelection, []SelectionSnapshot, []domain.Issue) {
	c.Applications = append([]domain.ApplicationID{}, c.Applications...)
	selections := []domain.ApplicationSelection{}
	snapshots := []SelectionSnapshot{}
	issues := []domain.Issue{}
	seen := map[domain.ApplicationID]bool{}
	for _, id := range c.Applications {
		seen[id] = true
	}
	for _, source := range c.ApplicationSets {
		apps, err := generated(ctx, source, inventories[source.Instance], c.Render.MaxApplications)
		selection := domain.ApplicationSelection{Source: source, Mode: "current-generated-applications", Applications: []domain.ApplicationID{}, Boundary: GeneratedSelectionBoundary}
		if err == nil {
			additional := 0
			for _, app := range apps {
				if !seen[app.Application.ID] {
					additional++
				}
			}
			if len(c.Applications)+additional > c.Render.MaxApplications {
				err = domain.LimitError(domain.CodeSelectionLimitExceeded, "selection", "combined explicit and generated Applications", "render.maxApplications", int64(len(c.Applications)+additional), int64(c.Render.MaxApplications), "Applications", false, domain.MaxRenderApplications)
			}
		}
		if err != nil {
			issues = append(issues, ContextualIssue(err, source, c.Git.RepositoryURL, ""))
		} else {
			selection.OwnerUID = apps[0].OwnerUID
			for _, app := range apps {
				id := app.Application.ID
				selection.Applications = append(selection.Applications, id)
				if !seen[id] {
					seen[id] = true
					c.Applications = append(c.Applications, id)
				}
			}
			data, _ := json.Marshal(apps)
			snapshots = append(snapshots, SelectionSnapshot{Source: source, Digest: sha256.Sum256(data)})
		}
		selections = append(selections, selection)
	}
	domain.SortApplicationIDs(c.Applications)
	return c, selections, snapshots, issues
}

func VerifySelections(ctx context.Context, c profile.Config, inventories map[string]domain.ApplicationInventory, snapshots []SelectionSnapshot) []domain.Issue {
	issues := []domain.Issue{}
	for _, snapshot := range snapshots {
		apps, err := generated(ctx, snapshot.Source, inventories[snapshot.Source.Instance], c.Render.MaxApplications)
		if err == nil {
			data, _ := json.Marshal(apps)
			if sha256.Sum256(data) != snapshot.Digest {
				err = domain.Fail(domain.CodeSelectionChanged, "selection", "generated Application membership, ownership or stored spec changed during analysis", "rerun after ApplicationSet reconciliation is stable", nil)
			}
		}
		if err != nil {
			issues = append(issues, ContextualIssue(err, snapshot.Source, c.Git.RepositoryURL, ""))
		}
	}
	return issues
}

func ApplySelectionReport(report *domain.Report, selections []domain.ApplicationSelection, issues []domain.Issue) {
	report.Selections = selections
	if len(selections) > 0 {
		report.Limitations = append([]string{GeneratedSelectionBoundary}, report.Limitations...)
	}
	ApplyRunIssues(report, issues)
}
