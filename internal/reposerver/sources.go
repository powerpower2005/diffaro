package reposerver

import (
	"context"
	"regexp"
	"strings"

	"diffaro/internal/domain"
)

var sourceRef = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// boundSource is one plan entry: the single-source Application rendered for
// it and its authorized binding.
type boundSource struct {
	plan       domain.SourceRevision
	app        domain.Application
	binding    *Binding
	valuesOnly bool
}

// RenderDesiredSources renders a proposed spec without looking up or creating
// an installed Application. Every source is authorized before any rendering
// against one AppProject read, which is read again after the last source
// rendered: a change anywhere in between is drift.
func (c *Client) RenderDesiredSources(ctx context.Context, app domain.Application, plan []domain.SourceRevision) ([]string, error) {
	if app.ID.Instance != c.API.Instance {
		return nil, otherInstance()
	}
	if !safeName.MatchString(app.ID.Name) {
		return nil, invalid("desired Application name is invalid")
	}
	sources, ok := app.Spec["sources"].([]any)
	if !ok || len(sources) == 0 || len(sources) > 32 || len(sources) != len(plan) {
		return nil, invalidMultiPlan()
	}
	if err := domain.SupportedSource(app); err != nil {
		return nil, err
	}
	bound, refs, project, err := c.authorizeSources(ctx, app, sources, plan)
	if err != nil {
		return nil, err
	}
	if err = checkRefs(bound, refs); err != nil {
		return nil, err
	}
	result, err := c.renderAll(ctx, bound, project, refs)
	if err != nil {
		return nil, err
	}
	if err = c.API.ProjectUnchanged(ctx, app.Project(), project, drift("AppProject changed during repo-server render")); err != nil {
		return nil, err
	}
	return result, nil
}

// authorizeSources binds every plan entry in order, authorizes it against
// the AppProject read once for the first entry, and collects the external
// value sources by ref.
func (c *Client) authorizeSources(ctx context.Context, app domain.Application, sources []any, plan []domain.SourceRevision) ([]boundSource, map[string]any, map[string]any, error) {
	bound := make([]boundSource, len(plan))
	refs := map[string]any{}
	var project map[string]any
	for i, item := range plan {
		if !item.Matches(sources[i], i) {
			return nil, nil, nil, domain.SourceFailure(invalidMultiPlan(), item)
		}
		copy := domain.Application{ID: app.ID, Spec: domain.CloneSpec(app.Spec)}
		delete(copy.Spec, "sources")
		copy.Spec["source"] = domain.CloneSpec(item.Source)
		binding, err := c.selectBinding(ctx, copy)
		if err == nil && project == nil {
			project, err = c.projectSpec(ctx, app.Project())
		}
		if err == nil {
			err = c.authorize(ctx, binding, project)
		}
		if err != nil {
			return nil, nil, nil, domain.SourceFailure(err, item)
		}
		bound[i] = boundSource{plan: item, app: copy, binding: binding, valuesOnly: item.Ref != "" && domain.String(item.Source, "path") == ""}
		if item.Ref != "" {
			key := "$" + item.Ref
			if !sourceRef.MatchString(item.Ref) || refs[key] != nil {
				return nil, nil, nil, domain.SourceFailure(invalidMultiPlan(), item)
			}
			refs[key] = map[string]any{"repo": domain.CloneSpec(binding.Request["repo"].(map[string]any)), "targetRevision": item.Revision}
		}
	}
	return bound, refs, project, nil
}

// checkRefs fails before rendering if an external value or file parameter
// names no source ref.
func checkRefs(bound []boundSource, refs map[string]any) error {
	for _, source := range bound {
		helm, _ := source.app.Source()["helm"].(map[string]any)
		files, _ := helm["valueFiles"].([]any)
		files = append([]any(nil), files...)
		parameters, _ := helm["fileParameters"].([]any)
		for _, raw := range parameters {
			p, _ := raw.(map[string]any)
			files = append(files, domain.String(p, "path"))
		}
		for _, raw := range files {
			file, _ := raw.(string)
			if strings.HasPrefix(file, "$") && refs[strings.SplitN(file, "/", 2)[0]] == nil {
				return domain.SourceFailure(domain.Fail(domain.CodeSourceInvalid, "render", "external Helm file references an undefined source", "define the referenced source ref", nil), source.plan)
			}
		}
	}
	return nil
}

// renderAll renders the sources in plan order within one combined
// manifest budget.
func (c *Client) renderAll(ctx context.Context, bound []boundSource, project, refs map[string]any) ([]string, error) {
	result := []string{}
	limit := c.maxResponseBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	var used int64
	for _, source := range bound {
		manifests, err := c.renderBoundSource(ctx, source.app, source.plan.Revision, source.binding, project, refs, true, source.valuesOnly)
		if err != nil {
			return nil, domain.SourceFailure(err, source.plan)
		}
		for _, raw := range manifests {
			used += int64(len(raw))
			if used > limit {
				return nil, domain.SourceFailure(domain.LimitError(domain.CodeLimitExceeded, "render", "combined multi-source target manifests", "render.maxResponseBytes", used, limit, "bytes", true, domain.MaxRenderResponseBytes), source.plan)
			}
		}
		result = append(result, manifests...)
	}
	return result, nil
}

func invalidMultiPlan() error {
	return domain.Fail(domain.CodeSourceInvalid, "render", "multi-source revision plan does not match supported Application sources", "rebuild an ordered plan preserving source inputs, unique refs and immutable revisions", nil)
}
