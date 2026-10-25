package argocd

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strconv"

	"diffaro/internal/domain"
)

// RenderSources sends all positions, including unchanged sources, so Argo CD
// cannot independently move a branch between the two requested targets.
func (c *Client) RenderSources(ctx context.Context, app domain.Application, plan []domain.SourceRevision) ([]string, error) {
	query, err := sourceQuery(app, plan)
	if err != nil {
		return nil, err
	}
	return c.renderQuery(ctx, app, query, false)
}

func sourceQuery(app domain.Application, plan []domain.SourceRevision) (url.Values, error) {
	sources, ok := app.Spec["sources"].([]any)
	if !ok || len(sources) == 0 || len(sources) != len(plan) || len(plan) > 32 {
		return nil, invalidSourcePlan()
	}
	query := url.Values{"appNamespace": {app.ID.Namespace}, "project": {app.Project()}}
	for i, item := range plan {
		if !item.Matches(sources[i], i) {
			return nil, invalidSourcePlan()
		}
		query.Add("sourcePositions", strconv.Itoa(item.Position))
		query.Add("revisions", item.Revision)
	}
	return query, nil
}

func (c *Client) RenderDesiredSources(ctx context.Context, desired domain.Application, plan []domain.SourceRevision) ([]string, error) {
	stored, err := c.desiredApplication(ctx, desired)
	if err != nil {
		var failure *domain.Failure
		if errors.As(err, &failure) && failure.Issue.Code == domain.CodeNotFound {
			return nil, desiredUnavailable(unreadableChildReason, err)
		}
		return nil, KnownApplicationAccess(err, desired)
	}
	if !reflect.DeepEqual(withoutSourceRevisions(stored.Spec), withoutSourceRevisions(desired.Spec)) {
		return nil, desiredUnavailable(specMismatchReason(withoutSourceRevisions(desired.Spec), withoutSourceRevisions(stored.Spec)), nil)
	}
	// Preview checks the AppProject of every multi-source target before
	// rendering it, so it is not read again here.
	query, err := sourceQuery(stored, plan)
	if err != nil {
		return nil, err
	}
	return c.renderQuery(ctx, stored, query, true)
}

func withoutSourceRevisions(spec map[string]any) map[string]any {
	copy := withoutRevision(spec)
	if sources, ok := copy["sources"].([]any); ok {
		for _, raw := range sources {
			if source, ok := raw.(map[string]any); ok {
				delete(source, "targetRevision")
			}
		}
	}
	return copy
}

func invalidSourcePlan() error {
	return domain.Fail(domain.CodeSourceInvalid, "render", "multi-source revision plan does not match the Application source positions and inputs", "rebuild the source plan from the frozen Application spec", nil)
}
