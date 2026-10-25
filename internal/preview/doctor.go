package preview

import (
	"context"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func Doctor(ctx context.Context, c profile.Config, git domain.GitProvider, renderers map[string]domain.Renderer) domain.Report {
	r := domain.NewReport()
	r.Coverage = domain.CoverageConnectionPreflight
	r.Revisions.Repository = c.Git.Repository
	r.Limitations = []string{"doctor checks read access only; it renders no targets, so it does not prove manifest generation or backend equivalence. Run doctor --render --pr NUMBER or diff to verify rendering."}
	_, err := git.Resolve(ctx, c.Git.Repository, "HEAD")
	if err != nil {
		i := ContextualIssue(err, domain.ApplicationID{}, c.Git.Repository, "")
		i.Application = nil
		r.Issues = append(r.Issues, i)
	}
	for _, id := range c.Applications {
		a, err := renderers[id.Instance].Application(ctx, id)
		if err == nil {
			err = Supported(a)
		}
		if err == nil {
			if check := capabilities(renderers[id.Instance]).project; check != nil {
				err = check.CheckProject(ctx, a)
			}
		}
		result := domain.ApplicationResult{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, Project: a.Project(), State: domain.StateUnchanged, Execution: domain.ExecutionComplete, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{}, Inputs: inputSummary(a.Source())}
		if err != nil {
			issue := ContextualIssue(err, id, c.Git.Repository, "")
			result.State = domain.StateFailed
			result.Execution = domain.ExecutionFailed
			if issue.Code == domain.CodeAccessDenied {
				result.State = domain.StateDenied
				result.Execution = domain.ExecutionDenied
			}
			result.Issues = append(result.Issues, issue)
		}
		r.Applications = append(r.Applications, result)
	}
	finishReport(&r, ctx.Err() != nil)
	return r
}
