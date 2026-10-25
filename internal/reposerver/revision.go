package reposerver

import (
	"context"

	"diffaro/internal/domain"
)

const resolveMethod = "repository.RepoServerService/ResolveRevision"

// ResolveSourceRevision uses only this instance's trusted repository credentials.
// It neither creates the desired Application nor uses credentials from its spec.
func (c *Client) ResolveSourceRevision(ctx context.Context, app domain.Application, repository, reference string) (string, error) {
	if app.ID.Instance != c.API.Instance || !safeName.MatchString(app.ID.Name) {
		return "", denied("invalid Application identity for source revision resolution")
	}
	matched := false
	sources, _ := app.Spec["sources"].([]any)
	if len(sources) == 0 {
		sources = []any{app.Source()}
	}
	for _, raw := range sources {
		source, _ := raw.(map[string]any)
		if domain.String(source, "repoURL") == repository && domain.String(source, "targetRevision") == reference {
			if domain.String(source, "chart") != "" || domain.String(source, "tagPrefix") != "" {
				return "", domain.Fail(domain.CodeUnsupportedSource, "revision", "revision resolution requires a Git source without tagPrefix", "use a supported Git source", nil)
			}
			matched = true
		}
	}
	if !matched {
		return "", invalid("revision request does not match an Application source")
	}
	// A reduced source is sufficient for Git resolution; Helm values never cross
	// this RPC boundary. Authorization still uses the original app scope.
	source := map[string]any{"repoURL": repository}
	copy := domain.Application{ID: app.ID, Spec: domain.CloneSpec(app.Spec)}
	delete(copy.Spec, "sources")
	copy.Spec["source"] = source
	binding, project, err := c.authorizedBinding(ctx, copy)
	if err != nil {
		return "", err
	}
	if c.resolve == nil {
		return "", invalid("repo-server revision resolver is unavailable")
	}
	if reference == "" {
		reference = "HEAD"
	}
	response, err := c.resolve(ctx, map[string]any{
		"repo":              domain.CloneSpec(binding.Request["repo"].(map[string]any)),
		"app":               map[string]any{"spec": map[string]any{"source": source}},
		"ambiguousRevision": reference, "sourceIndex": 0, "noRevisionCache": true,
	})
	if err != nil {
		return "", err
	}
	if !domain.IsCommitSHA(response.Revision) {
		return "", domain.Fail(domain.CodeRevisionInvalid, "revision", "repo-server did not resolve an immutable Git commit", "check the source reference and repo-server compatibility", nil)
	}
	if err = c.API.ProjectUnchanged(ctx, app.Project(), project, drift("AppProject changed during revision resolution")); err != nil {
		return "", err
	}
	return response.Revision, nil
}
