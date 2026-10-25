package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	revisions "diffaro/internal/revision"
)

// Default texts of the recursive limitations whose wording depends on the
// render backend (BackendLimitations).
const (
	basicAnalysisLimitation = domain.LimitationBasicAnalysis
	secretPayloadLimitation = domain.LimitationSecretPayload
)

// BackendLimitations words the recursive limitations that depend on the
// render backend. An empty field keeps the default text.
type BackendLimitations struct {
	BasicAnalysis string
	SecretPayload string
}

// recursiveLimitations are always reported for recursive comparisons, the
// backend-dependent ones worded by texts.
func recursiveLimitations(texts BackendLimitations) []string {
	return []string{
		domain.LimitationRootSnapshot,
		domain.LimitationOtherRepositories,
		cmp.Or(texts.BasicAnalysis, basicAnalysisLimitation),
		domain.LimitationMissingNodes,
		domain.LimitationRemovedTargets,
		cmp.Or(texts.SecretPayload, secretPayloadLimitation),
	}
}

// RunRecursive compares two independently discovered target graphs. The stored
// root specs are frozen once; child specs always come from the parent's target.
func (r Runner) RunRecursive(ctx context.Context, pair domain.RevisionPair) domain.Report {
	return r.newGraphRun(pair, nil, nil).compareGraphs(ctx, nil)
}

// compareGraphs starts from the profile's explicit roots when candidates is
// nil. A non-nil (possibly empty) candidates list comes from repository
// discovery; roots are then selected from the candidates' own graphs.
// Stages: roots → walkBoth → graphSummary (edges and faults) → compareNodes
// (compareNode per Application, retained) → finishReport.
func (r *run) compareGraphs(ctx context.Context, candidates []domain.Application) domain.Report {
	report := domain.NewReport()
	report.RenderBackends = backendInfo(r.backends)
	report.Revisions, report.Coverage = r.pair, "recursive-target-graph"
	report.Limitations = recursiveLimitations(r.limitations)
	var roots []domain.Application
	if candidates != nil {
		roots = r.selectDiscoveredRoots(ctx, candidates)
		report.Coverage = "repository-discovered-target-graph"
	} else {
		roots = r.loadRoots(ctx)
	}
	base, head := r.walkBoth(ctx, roots)
	var faults []domain.Issue
	report.Graph, faults = graphSummary(base, head)
	report.Issues = append(report.Issues, faults...)
	report.Applications = append(report.Applications, r.compareNodes(ctx, base, head)...)
	finishReport(&report, ctx.Err() != nil)
	noteObservedScopes(&report)
	noteIgnoreRules(&report)
	return report
}

// walkBoth walks the base and head graphs from roots and records them for
// descendant selection.
func (r *run) walkBoth(ctx context.Context, roots []domain.Application) (base, head graph.Snapshot) {
	pair := r.pair
	bounded(2, r.cfg.Render.MaxConcurrency, func(i int) {
		if i == 0 {
			base = r.walk(traceSide(ctx, "base", pair.Base, "final"), "base", pair.Base, roots)
		} else {
			head = r.walk(traceSide(ctx, "head", pair.Head, "final"), "head", pair.Head, roots)
		}
	})
	if r.descendants != nil {
		r.descendants.record(base, head)
	}
	return base, head
}

// graphSummary states both graphs' completeness, edges and edge changes,
// and lists each side's discovery faults as run issues.
func graphSummary(base, head graph.Snapshot) (*domain.GraphSummary, []domain.Issue) {
	summary := &domain.GraphSummary{BaseComplete: base.Complete, HeadComplete: head.Complete, BaseEdges: base.Edges, HeadEdges: head.Edges}
	summary.Changes = graph.CompareEdges(base, head)
	var faults []domain.Issue
	for _, side := range []struct {
		name     string
		snapshot graph.Snapshot
	}{{"base", base}, {"head", head}} {
		for _, fault := range side.snapshot.Faults {
			faults = append(faults, sideIssue(fault.Err, fault.ID, fault.Repository, fault.Revision, side.name))
		}
	}
	return summary, faults
}

// compareNodes compares every Application of either graph in identity
// order. Results beyond the retained-byte budget are replaced by a limit
// failure.
func (r *run) compareNodes(ctx context.Context, base, head graph.Snapshot) []domain.ApplicationResult {
	union := map[domain.ApplicationID]bool{}
	for id := range base.Nodes {
		union[id] = true
	}
	for id := range head.Nodes {
		union[id] = true
	}
	var ids []domain.ApplicationID
	for id := range union {
		ids = append(ids, id)
	}
	domain.SortApplicationIDs(ids)
	budget := newRetainBudget(r.maxResultBytes)
	baseInstances := completeInstances(base, r.cfg.Routing, r.cfg.Instances)
	headInstances := completeInstances(head, r.cfg.Routing, r.cfg.Instances)
	var apps []domain.ApplicationResult
	for _, id := range ids {
		endDiff := domain.TraceStart(domain.TraceApplication(ctx, id), "diff", "application", "")
		app, err := guarded("diff", func() (domain.ApplicationResult, error) {
			return compareNode(base.Nodes[id], head.Nodes[id], nodePresence(base, head, baseInstances, headInstances, id), r.redactor), nil
		})
		endDiff(err)
		if err != nil {
			// A panic fails this node only.
			root, path := id, []domain.ApplicationID{id}
			for _, node := range []*graph.Node{head.Nodes[id], base.Nodes[id]} {
				if node != nil {
					root, path = node.Root, node.Path
					break
				}
			}
			app = domain.ApplicationResult{ID: id, Root: root, GraphPath: path, State: domain.StateUnknown, Execution: domain.ExecutionFailed, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{ContextualIssue(err, id, "", "")}, Inputs: inputSummary(nil)}
		}
		if !budget.keep(app) {
			app = domain.ApplicationResult{ID: id, Root: app.Root, GraphPath: app.GraphPath, State: domain.StateUnknown, Execution: domain.ExecutionFailed, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{budget.limitIssue("retained graph comparison results", id, "")}, Inputs: inputSummary(nil)}
		}
		apps = append(apps, app)
	}
	return apps
}

// loadRoots reads the profile's explicit root Applications once. Lookup
// failures are kept per root in rootErrors so independent roots still compare.
// Lookups run within render.maxConcurrency; the root spec budget is then
// charged in identity order, as in a sequential run.
func (r *run) loadRoots(ctx context.Context) []domain.Application {
	ids := append([]domain.ApplicationID{}, r.cfg.Applications...)
	domain.SortApplicationIDs(ids)
	roots := make([]domain.Application, len(ids))
	errs := make([]error, len(ids))
	bounded(len(ids), r.cfg.Render.MaxConcurrency, func(i int) {
		id := ids[i]
		roots[i] = domain.Application{ID: id}
		if b := r.backends[id.Instance]; b == nil {
			errs[i] = domain.Fail(domain.CodeRoutingInvalid, "lookup", "root instance has no renderer", "configure the root instance", nil)
		} else {
			stage, cancel := context.WithTimeout(domain.TraceApplication(ctx, id), r.cfg.Timeout())
			roots[i], errs[i] = lookupApplication(stage, b, id)
			cancel()
		}
	})
	rootBytes := 0
	for i, id := range ids {
		app, err := roots[i], errs[i]
		if err == nil {
			spec, e := json.Marshal(app.Spec)
			if e != nil || rootBytes+len(spec) > domain.RetainedByteLimit {
				err = domain.LimitError(domain.CodeGraphLimitExceeded, "lookup", "root Application specs", "root retained-byte budget", int64(rootBytes+len(spec)), domain.RetainedByteLimit, "bytes", true, 0)
			} else {
				rootBytes += len(spec)
			}
		}
		if err != nil {
			app = domain.Application{ID: id}
			r.rootErrors[id] = err
		}
		roots[i] = app
	}
	return roots
}

// selectDiscoveredRoots walks each repository candidate on both revisions and
// keeps only candidates that no other candidate's graph owns. Candidates
// whose ownership cannot be decided stay roots with a root error.
func (r *run) selectDiscoveredRoots(ctx context.Context, roots []domain.Application) []domain.Application {
	pair := r.pair
	walk := func(side, revision string, starting []domain.Application) graph.Snapshot {
		return r.walk(traceSide(ctx, side, revision, "probe"), side, revision, starting)
	}
	// Retain identities only: full candidate render snapshots are discarded
	// after each probe. The final walk preserves actual specs and resources.
	identities := func(s graph.Snapshot) graph.Snapshot {
		out := graph.Snapshot{Nodes: map[domain.ApplicationID]*graph.Node{}}
		for id := range s.Nodes {
			out.Nodes[id] = nil
		}
		return out
	}
	// Probes are independent; indexed results keep root selection ordered.
	candidates := make([]graph.CandidateGraphs, len(roots))
	bounded(len(roots), r.cfg.Render.MaxConcurrency, func(i int) {
		app := roots[i]
		candidates[i] = graph.CandidateGraphs{Application: app, Base: identities(walk("base", pair.Base, []domain.Application{app})), Head: identities(walk("head", pair.Head, []domain.Application{app}))}
	})
	var unresolved []domain.ApplicationID
	roots, unresolved = graph.SelectRoots(candidates)
	// The probes have returned; root errors are only read from here on.
	for _, id := range unresolved {
		r.rootErrors[id] = domain.Fail(domain.CodeSelectionAmbiguous, "selection", "candidate ownership is cyclic or reverses between revisions", "select explicit roots or resolve the conflicting ownership", nil)
		roots = append(roots, domain.Application{ID: id})
	}
	return roots
}

// walk discovers the graph of one side, at revision, from the given roots.
func (r *run) walk(ctx context.Context, side, revision string, starting []domain.Application) graph.Snapshot {
	w := graph.Walker{MaxDepth: r.cfg.Render.MaxDepth, MaxApplications: r.cfg.Render.MaxApplications, Route: r.cfg.Routing.Route}
	expand := func(parent context.Context, app domain.Application, root bool) (graph.Expansion, error) {
		return r.tracedExpand(parent, app, root, revision)
	}
	w.Expand = r.cache.wrap(revision, expand)
	// Wide App-of-Apps render their children concurrently; the walk itself
	// stays ordered. Prefetches end with the walk.
	prefetchCtx, cancel := context.WithCancel(ctx)
	var prefetches sync.WaitGroup
	defer prefetches.Wait()
	defer cancel()
	w.Prefetch = func(_ context.Context, children []domain.Application) {
		r.cache.prefetch(prefetchCtx, &prefetches, revision, expand, children)
	}
	if r.descendants != nil {
		otherSide, other := "head", r.descendants.pair.Head
		if side == "head" {
			otherSide, other = "base", r.descendants.pair.Base
		}
		opposite := r.cache.wrap(other, func(parent context.Context, app domain.Application, root bool) (graph.Expansion, error) {
			return r.tracedExpand(traceSide(parent, otherSide, other, "opposite"), app, root, other)
		})
		w.Skip = r.descendants.walkSkip(revision, opposite)
	}
	return w.Walk(ctx, starting)
}

// tracedExpand scopes trace steps to the node and records its outcome.
func (r *run) tracedExpand(parent context.Context, app domain.Application, root bool, revision string) (graph.Expansion, error) {
	ctx := domain.TraceApplication(parent, app.ID)
	origin := "desired"
	if root {
		origin = "stored"
	}
	// A child's spec is its parent's rendered target; its digest shows
	// whether this render started from the same input as another run.
	domain.TraceRead(ctx, "lookup", "spec", origin)(func() string { return domain.DigestJSON(app.Spec) }, nil)
	exp, err := guarded("render", func() (graph.Expansion, error) { return r.expand(ctx, app, root, revision) })
	domain.TraceStart(ctx, "result", "application", exp.Revision)(err)
	return exp, err
}

// expand renders one graph node at revision: root error → revision mapping
// of a single source from another repository → prepareTarget → (stage
// timeout) planSide → checkProject when projectChecked → renderSide →
// parseSide. Root nodes use stored specs; children use desired specs.
func (r *run) expand(parent context.Context, app domain.Application, root bool, revision string) (graph.Expansion, error) {
	exp := graph.Expansion{Repository: domain.String(app.Source(), "repoURL"), Revision: revision, RevisionRule: "selected-repository-pair"}
	if root && r.rootErrors[app.ID] != nil {
		exp.Repository = r.cfg.Git.RepositoryURL
		return exp, r.rootErrors[app.ID]
	}
	if multipleSources(app) {
		exp.Repository = r.cfg.Git.RepositoryURL
		exp.RevisionRule = "per-source-plan"
	} else if r.rendersAtOwnCommit(app) {
		exp.Revision, exp.RevisionRule = domain.String(app.Source(), "targetRevision"), "source-commit"
		// PlanSources is the single source of the non-PR revision rules
		// (pinned SHA, tagPrefix, resolver), shared with multi-source nodes.
		plan, err := revisions.PlanSources(parent, app, r.cfg.Git.RepositoryURL, revision, r.tracedSourceResolver(app), r.cfg.Git.Aliases()...)
		if err != nil {
			return exp, withoutSourcePosition(err)
		}
		exp.Revision, exp.RevisionRule, exp.RequestedRevision = plan[0].Revision, plan[0].Rule, plan[0].Requested
	}
	t, err := r.prepareTarget(app.ID, app, root)
	if err != nil {
		return exp, err
	}
	stage, cancel := context.WithTimeout(parent, r.cfg.Timeout())
	defer cancel()
	side, err := r.planSide(stage, t, exp.Revision)
	exp.Sources = side.sources
	if err != nil {
		return exp, err
	}
	if t.projectChecked() {
		if err := checkProject(stage, t); err != nil {
			return exp, err
		}
	}
	raw, renderErr := renderSide(stage, t, side)
	if renderErr != nil && !domain.IsRenderPartial(renderErr) {
		return exp, renderErr
	}
	endParse := domain.TraceStart(stage, "parse", "target", "")
	parsed, err := r.parseSide(t, raw)
	exp.Resources, exp.RepeatedResources = parsed.resources, parsed.repeated
	if stage.Err() != nil {
		endParse(stage.Err())
		return exp, stage.Err()
	}
	endParse(err)
	if err != nil {
		return exp, err
	}
	return exp, renderErr
}
