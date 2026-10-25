package preview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// discoveryScope is an explicit inventory scope, not an inferred root. Empty
// Projects means all projects visible to the instance's read-only token.
type discoveryScope struct {
	Instance string   `json:"instance"`
	Projects []string `json:"projects"`
}

type discoverySnapshot struct {
	Scope        discoveryScope
	Applications []domain.Application
	Decisions    []domain.DiscoveryDecision
	Digest       [32]byte
	// ManagesApplications keeps the inventory's stored status hint for
	// changed-path selection; absent entries are unknown.
	ManagesApplications map[domain.ApplicationID]bool
	// Scopes aggregates the inventory's observed resource scopes per
	// destination cluster for resources rendered without a namespace.
	Scopes clusterScopes
	// OtherHosts counts unselected Applications in the project filter whose
	// source has the PR repository's path on another host (OtherHostSources).
	OtherHosts map[string]int
}

// discoverRepository deliberately selects every Application whose source names
// the changed repository. It does not guess independence from a path prefix:
// values, local subcharts and other shared inputs can cross chart directories.
// Multi-source candidates are selected when any source names the repository.
// aliases are explicitly configured URLs of the same repository.
func discoverRepository(ctx context.Context, scope discoveryScope, repository string, inventory domain.ApplicationInventory, limit int, aliases ...string) (discoverySnapshot, error) {
	out := discoverySnapshot{Scope: scope, Applications: []domain.Application{}, Decisions: []domain.DiscoveryDecision{}}
	if scope.Instance == "" || repository == "" || limit < 1 {
		return out, domain.Fail(domain.CodeConfigurationInvalid, "selection", "discovery requires an instance, repository and positive limit", "provide an explicit discovery scope", nil)
	}
	if inventory == nil {
		return out, domain.Fail(domain.CodeSelectionUnavailable, "selection", "Application inventory is unavailable for this instance", "configure readable Argo CD access", nil)
	}
	items, err := inventory.ListApplications(ctx, "", limit)
	if err != nil {
		return out, err
	}
	if len(items) > limit {
		return out, domain.LimitError(domain.CodeSelectionLimitExceeded, "selection", "readable Application inventory", "render.maxApplications", int64(len(items)), int64(limit), "Applications", false, domain.MaxRenderApplications)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Application.ID.String() < items[j].Application.ID.String() })
	seen := map[domain.ApplicationID]bool{}
	// Hash what selection reads from every visible Application: identity,
	// project, source repositories, and the full stored spec of Applications
	// that use the PR repository. An app that was previously unrelated can
	// change its source during the run and must not silently escape selection;
	// other spec changes of unrelated apps (such as image updates) cannot
	// change the selection or the frozen root specs.
	frozen := make([]domain.Application, 0, len(items))
	selected := make([]inventoryEntry, 0, len(items))
	for _, item := range items {
		app := item.Application
		if app.ID.Instance != scope.Instance || app.ID.Name == "" || app.ID.Namespace == "" || app.Spec == nil || seen[app.ID] {
			return discoverySnapshot{Scope: scope, Applications: []domain.Application{}, Decisions: []domain.DiscoveryDecision{}}, domain.Fail(domain.CodeResponseInvalid, "selection", "inventory contains an invalid, duplicate or cross-instance Application", "check the inventory adapter", nil)
		}
		seen[app.ID] = true
		app.Spec = domain.CloneSpec(app.Spec)
		frozen = append(frozen, app)
		references := referencesRepository(app, repository, aliases...)
		decision := domain.DiscoveryDecision{ID: app.ID, Project: app.Project(), Reason: "different-source-repository"}
		projectAllowed := len(scope.Projects) == 0
		for _, project := range scope.Projects {
			projectAllowed = projectAllowed || project == app.Project()
		}
		if !projectAllowed {
			decision.Reason = "outside-project-filter"
		} else if references {
			decision.Selected, decision.Reason = true, "same-source-repository-conservative-selection"
			out.Applications = append(out.Applications, app)
		} else {
			for _, source := range otherHostSources(app, repository, aliases) {
				if out.OtherHosts == nil {
					out.OtherHosts = map[string]int{}
				}
				out.OtherHosts[source]++
			}
		}
		// Descendant selection also needs the hint of Applications
		// from other repositories.
		if item.ManagesApplications != nil {
			if out.ManagesApplications == nil {
				out.ManagesApplications = map[domain.ApplicationID]bool{}
			}
			out.ManagesApplications[app.ID] = *item.ManagesApplications
		}
		out.Decisions = append(out.Decisions, decision)
		entry := inventoryEntry{ID: app.ID, Project: app.Project(), Repositories: sourceRepositories(app)}
		if references {
			entry.Spec = app.Spec
		}
		selected = append(selected, entry)
	}
	data, err := json.Marshal(frozen)
	if err != nil || len(data) > domain.RetainedByteLimit {
		// Every decision was made; the caller reports none as selected.
		return discoverySnapshot{Scope: scope, Applications: []domain.Application{}, Decisions: out.Decisions}, domain.LimitError(domain.CodeSelectionLimitExceeded, "selection", "stored Application inventory", "inventory retained-byte budget", int64(len(data)), domain.RetainedByteLimit, "bytes", false, 0)
	}
	data, err = json.Marshal(selected)
	if err != nil {
		return discoverySnapshot{Scope: scope, Applications: []domain.Application{}, Decisions: []domain.DiscoveryDecision{}}, domain.Fail(domain.CodeResponseInvalid, "selection", "inventory cannot be encoded for verification", "check the inventory adapter", err)
	}
	out.Digest = sha256.Sum256(data)
	out.Scopes = aggregateScopes(scope.Instance, frozen)
	return out, nil
}

// inventoryEntry is the selection-relevant projection of one Application.
type inventoryEntry struct {
	ID           domain.ApplicationID `json:"id"`
	Project      string               `json:"project"`
	Repositories []string             `json:"repositories"`
	Spec         map[string]any       `json:"spec,omitempty"`
}

// sourceRepositories lists source repository URLs in spec order.
func sourceRepositories(app domain.Application) []string {
	out := []string{}
	if sources := app.Sources(); len(sources) > 0 {
		for _, value := range sources {
			source, _ := value.(map[string]any)
			out = append(out, domain.String(source, "repoURL"))
		}
		return out
	}
	return append(out, domain.String(app.Source(), "repoURL"))
}

func referencesRepository(app domain.Application, repository string, aliases ...string) bool {
	if sources := app.Sources(); len(sources) > 0 {
		for _, value := range sources {
			if source, ok := value.(map[string]any); ok && profile.SameRepositoryAny(domain.String(source, "repoURL"), repository, aliases) {
				return true
			}
		}
		return false
	}
	return profile.SameRepositoryAny(domain.String(app.Source(), "repoURL"), repository, aliases)
}

func verifyDiscovery(ctx context.Context, before discoverySnapshot, repository string, inventory domain.ApplicationInventory, limit int, aliases ...string) error {
	after, err := discoverRepository(ctx, before.Scope, repository, inventory, limit, aliases...)
	if err != nil {
		return err
	}
	if after.Digest != before.Digest {
		return domain.Fail(domain.CodeSelectionChanged, "selection", "Application inventory membership or stored specs changed during analysis", "rerun when the Argo CD inventory is stable", nil)
	}
	return nil
}
