package argocd

import (
	"context"
	"net/url"
	"reflect"
	"sort"

	"diffaro/internal/domain"
)

// ListApplications returns only the server's RBAC-visible snapshot. It never
// treats labels or name prefixes as ApplicationSet ownership.
func (c *Client) ListApplications(ctx context.Context, namespace string, limit int) ([]domain.OwnedApplication, error) {
	var response struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				Owners    []struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
					Name       string `json:"name"`
					UID        string `json:"uid"`
					Controller bool   `json:"controller"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec   map[string]any `json:"spec"`
			Status struct {
				Resources    *[]statusResource `json:"resources"`
				ReconciledAt string            `json:"reconciledAt"`
				Sync         syncStatus        `json:"sync"`
			} `json:"status"`
		} `json:"items"`
	}
	q := url.Values{}
	if namespace != "" {
		q.Set("appNamespace", namespace)
	}
	if err := c.get(ctx, "/api/v1/applications", q, &response, "Application inventory lookup", "/api/v1/applications", "applications/get for the Applications to discover; inventory contains only RBAC-visible Applications", inventoryAccess(namespace)); err != nil {
		return nil, err
	}
	if response.Metadata.Continue != "" {
		return nil, domain.Fail(domain.CodeSelectionLimitExceeded, "selection", "Argo CD Application inventory is paginated; the complete inventory was not read", "narrow the inventory namespace; increasing maxApplications alone does not complete a paginated response", nil)
	}
	if len(response.Items) > limit {
		err := domain.LimitError(domain.CodeSelectionLimitExceeded, "selection", "Argo CD instance "+c.Instance+" readable Application inventory", "render.maxApplications", int64(len(response.Items)), int64(limit), "Applications", false, domain.MaxRenderApplications)
		failure := err.(*domain.Failure)
		failure.Issue.Action += "; for init use --max-applications; for doctor/diff edit render.maxApplications in the profile"
		return nil, err
	}
	result := []domain.OwnedApplication{}
	seen := map[domain.ApplicationID]bool{}
	for _, item := range response.Items {
		id := domain.ApplicationID{Instance: c.Instance, Namespace: item.Metadata.Namespace, Name: item.Metadata.Name}
		if id.Name == "" || id.Namespace == "" || item.Spec == nil || seen[id] || (namespace != "" && id.Namespace != namespace) {
			return nil, domain.Fail(domain.CodeResponseInvalid, "selection", "Application inventory contains missing, duplicate or out-of-scope identities", "check Argo CD API namespace filtering", nil)
		}
		seen[id] = true
		app := domain.OwnedApplication{Application: domain.Application{ID: id, Spec: item.Spec}}
		if item.Status.Resources != nil {
			manages := false
			for _, resource := range *item.Status.Resources {
				manages = manages || (resource.Group == "argoproj.io" && (resource.Kind == "Application" || resource.Kind == "ApplicationSet"))
			}
			app.ManagesApplications = &manages
			app.Application.ObservedScopes = observedScopes(*item.Status.Resources)
		} else if item.Status.Sync.empty(item.Spec, item.Status.ReconciledAt) {
			manages := false
			app.ManagesApplications = &manages
		}
		for _, owner := range item.Metadata.Owners {
			if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "ApplicationSet" && owner.Controller {
				if app.OwnerName != "" || owner.Name == "" || owner.UID == "" {
					return nil, domain.Fail(domain.CodeResponseInvalid, "selection", "ApplicationSet controller ownership is ambiguous or incomplete", "check generated Application ownerReferences", nil)
				}
				app.OwnerName, app.OwnerUID = owner.Name, owner.UID
			}
		}
		result = append(result, app)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Application.ID.String() < result[j].Application.ID.String() })
	return result, nil
}

// syncStatus is the part of Application status that shows what the last
// comparison used.
type syncStatus struct {
	Status     string `json:"status"`
	ComparedTo struct {
		Source      any `json:"source"`
		Sources     any `json:"sources"`
		Destination any `json:"destination"`
	} `json:"comparedTo"`
}

// empty reports an Application that Argo CD compared with its current spec
// and found in sync without tracked resources. Argo CD omits an empty
// status.resources list, so such an Application manages no children.
func (s syncStatus) empty(spec map[string]any, reconciledAt string) bool {
	return reconciledAt != "" && s.Status == "Synced" &&
		reflect.DeepEqual(s.ComparedTo.Source, spec["source"]) &&
		reflect.DeepEqual(s.ComparedTo.Sources, spec["sources"]) &&
		reflect.DeepEqual(s.ComparedTo.Destination, spec["destination"])
}
