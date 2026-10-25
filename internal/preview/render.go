package preview

import (
	"context"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	revisions "diffaro/internal/revision"
)

// plannedSide is what one revision of a prepared target renders from.
type plannedSide struct {
	// revision is the revision rendered: the requested revision, or a
	// single source's own resolved commit.
	revision string
	// plan pins every source of a multi-source target; nil otherwise.
	plan []domain.SourceRevision
	// sources reports the plan's provenance.
	sources []domain.SourceSummary
}

// planSide plans a multi-source target's per-source revisions and requires
// the multi-source capability; the plan's provenance is kept when the
// capability is missing. A single-source target renders at revision.
func (r *run) planSide(ctx context.Context, t preparedTarget, revision string) (plannedSide, error) {
	side := plannedSide{revision: revision}
	if !t.multiSource {
		return side, nil
	}
	end := domain.TraceStart(ctx, "revision", "plan-sources", revision)
	plan, err := guarded("revision", func() ([]domain.SourceRevision, error) {
		return revisions.PlanSources(ctx, t.app, r.cfg.Git.RepositoryURL, revision, r.sourceResolver(t.app), r.cfg.Git.Aliases()...)
	})
	end(err)
	if err != nil {
		return side, err
	}
	origin := "stored-application-spec"
	if !t.root {
		origin = "parent-target-manifest"
	}
	side.plan, side.sources = plan, sourceSummaries(plan, origin)
	if t.backend.multi == nil {
		return side, multiSourceUnavailable()
	}
	return side, nil
}

// checkProject verifies through the backend, when it can, that the
// Application's AppProject permits the comparison.
func checkProject(ctx context.Context, t preparedTarget) error {
	if t.backend.project == nil {
		return nil
	}
	end := domain.TraceStart(ctx, "project", "check", t.app.Project())
	err := func() (err error) {
		defer domain.RecoverAs(&err, "permission")
		return t.backend.project.CheckProject(ctx, t.app)
	}()
	end(err)
	return err
}

// renderSide renders one planned revision: stored specs through Render or
// RenderSources, desired child specs through RenderDesired or
// RenderDesiredSources. A single-source child whose backend cannot render a
// desired spec is skipped (basic analysis).
func renderSide(ctx context.Context, t preparedTarget, side plannedSide) ([]string, error) {
	var kind string
	var render func() ([]string, error)
	switch {
	case t.multiSource && t.root:
		kind, render = "stored-sources", func() ([]string, error) { return t.backend.multi.RenderSources(ctx, t.app, side.plan) }
	case t.multiSource:
		kind, render = "desired-sources", func() ([]string, error) { return t.backend.multi.RenderDesiredSources(ctx, t.app, side.plan) }
	case t.root:
		kind, render = "stored", func() ([]string, error) { return t.backend.Render(ctx, t.app, side.revision) }
	case t.backend.desired != nil:
		kind, render = "desired", func() ([]string, error) { return t.backend.desired.RenderDesired(ctx, t.app, side.revision) }
	default:
		return nil, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
	}
	end := domain.TraceRead(ctx, "render", kind, side.revision)
	raw, err := guarded("render", render)
	end(func() string { return domain.DigestJSON(raw) }, err)
	return raw, err
}

// parsedSide is one revision's targets keyed by resource identity.
type parsedSide struct {
	resources map[string]manifest.Resource
	repeated  []domain.ResourceID
}

// parseSide follows Argo CD's target deduplication for single and multiple
// sources: the last rendered occurrence wins and repeats stay explicit.
// Resources rendered without a namespace take their scope from the
// Application's observation, completed in repository discovery by its
// destination cluster's aggregate.
func (r *run) parseSide(t preparedTarget, raw []string) (parsedSide, error) {
	cluster := t.id.Instance + ":" + t.cluster
	resources, repeated, err := manifest.ParseMultiSource(raw, cluster, t.namespace, r.limits(), r.scopes.scopesFor(t.app, cluster))
	return parsedSide{resources: resources, repeated: repeated}, err
}

func sourceSummaries(plan []domain.SourceRevision, origin string) []domain.SourceSummary {
	result := make([]domain.SourceSummary, 0, len(plan))
	for _, source := range plan {
		inputs := inputSummary(source.Source)
		inputs.Origin = origin
		result = append(result, domain.SourceSummary{Position: source.Position, Repository: source.Repository, Revision: source.Revision, Ref: source.Ref, Rule: source.Rule, RequestedRevision: source.Requested, Inputs: inputs})
	}
	return result
}

// sideIssues lists one side's issues for err: a partial render keeps each
// per-source failure, any other error is one issue.
func sideIssues(err error, id domain.ApplicationID, repository, revision, side string) []domain.Issue {
	if !domain.IsRenderPartial(err) {
		return []domain.Issue{sideIssue(err, id, repository, revision, side)}
	}
	var issues []domain.Issue
	for _, failure := range domain.RenderIssues(err) {
		issues = append(issues, sideIssue(&domain.Failure{Issue: failure}, id, repository, revision, side))
	}
	return issues
}

func sideIssue(err error, id domain.ApplicationID, repository, revision, side string) domain.Issue {
	issue := ContextualIssue(err, id, repository, revision)
	issue.Side = side
	return issue
}
