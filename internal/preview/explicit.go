package preview

import (
	"context"
	"errors"
	"sync/atomic"

	"diffaro/internal/domain"
)

const otherRepositorySkip = "source repository differs from the explicitly selected repository"

// Run compares the explicitly configured Applications in a rolling window of
// render.maxConcurrency slots. Results are offered to the retained-byte
// budget in Application order, so the retained prefix does not depend on
// backend latency.
func (r Runner) Run(ctx context.Context, pair domain.RevisionPair) domain.Report {
	x := r.newRun(pair)
	report := domain.NewReport()
	report.RenderBackends = backendInfo(x.backends)
	report.Revisions = pair
	report.Limitations = []string{domain.LimitationExplicitScope, domain.LimitationExplicitSecretPayload, domain.LimitationExplicitBackend}
	ids := append([]domain.ApplicationID{}, x.cfg.Applications...)
	domain.SortApplicationIDs(ids)
	report.Applications = x.applications(ctx, ids)
	finishReport(&report, ctx.Err() != nil)
	noteObservedScopes(&report)
	noteIgnoreRules(&report)
	return report
}

// applications compares ids with at most render.maxConcurrency slots in use.
// Each Application holds one slot from start to end, and its head side holds
// a second one while it renders alongside the base side (application). A new
// Application starts as soon as a slot is free; none starts once the
// retained-byte budget is exhausted. Results are offered to the budget in
// index order as they become available, so the kept prefix, every result
// and every issue are those of a sequential run.
func (r *run) applications(ctx context.Context, ids []domain.ApplicationID) []domain.ApplicationResult {
	results := make([]domain.ApplicationResult, len(ids))
	done := make([]chan struct{}, len(ids))
	for i := range done {
		done[i] = make(chan struct{})
	}
	slots := make(chan struct{}, max(r.cfg.Render.MaxConcurrency, 1))
	var exhausted atomic.Bool
	go func() {
		for i, id := range ids {
			slots <- struct{}{}
			if exhausted.Load() {
				// Replaced by the budget below; not rendered.
				<-slots
				close(done[i])
				continue
			}
			go func() {
				defer func() { <-slots; close(done[i]) }()
				results[i] = r.application(ctx, id, slots)
			}()
		}
	}()
	budget := newRetainBudget(r.maxResultBytes)
	for i, id := range ids {
		<-done[i]
		if !budget.keep(results[i]) {
			exhausted.Store(true)
			results[i] = domain.ApplicationResult{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, State: domain.StateUnknown, Execution: domain.ExecutionFailed, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{budget.limitIssue("retained Application comparison results", id, r.cfg.Git.RepositoryURL)}, Inputs: inputSummary(nil)}
		}
	}
	return results
}

// alongside starts fn on its own goroutine when slots has a free slot, which
// fn holds until it returns, and returns a wait function that returns once fn
// is done and raises fn's panic in the caller. Without a free slot
// it returns nil and the caller runs fn itself later.
func alongside(slots chan struct{}, fn func()) func() {
	select {
	case slots <- struct{}{}:
	default:
		return nil
	}
	done := make(chan struct{})
	var panicked panicSlot
	go func() {
		defer func() { <-slots; close(done) }()
		defer panicked.capture()
		fn()
	}()
	return func() {
		<-done
		panicked.raise()
	}
}

// headSide is the head revision's plan and render.
type headSide struct {
	plan      plannedSide
	planErr   error
	raw       []string
	renderErr error
}

// renderedSide is one side's raw targets and its render error: nil or a
// partial render.
type renderedSide struct {
	raw []string
	err error
}

// explicitResult is one explicit comparison's result while it is built.
type explicitResult struct {
	// ctx scopes trace steps to the Application.
	ctx        context.Context
	repository string
	pair       domain.RevisionPair
	out        domain.ApplicationResult
	// failedSides are the side failures already in out.Issues; with a new
	// failure they decide the execution (executionFor).
	failedSides []error
}

// fail ends the comparison with err on side, "base", "head" or "" when the
// failure belongs to neither, such as the lookup. A skip ends it skipped,
// unless a side already failed.
func (res *explicitResult) fail(err error, side string) domain.ApplicationResult {
	revision := ""
	switch side {
	case "base":
		revision = res.pair.Base
	case "head":
		revision = res.pair.Head
	}
	domain.TraceStart(res.ctx, "result", "application", revision)(err)
	var skipped *domain.RenderSkipped
	if errors.As(err, &skipped) && len(res.failedSides) > 0 {
		// A skipped head does not hide a failing base side: the result
		// stays failed with its issues, as a graph node does.
		return res.out
	}
	if errors.As(err, &skipped) {
		res.out.State = domain.StateSkipped
		res.out.Execution = domain.ExecutionSkipped
		res.out.SkipReason = skipped.Reason
		return res.out
	}
	issue := ContextualIssue(err, res.out.ID, res.repository, revision)
	issue.Side = side
	res.out.Issues = append(res.out.Issues, issue)
	res.out.Execution = executionFor(append(res.failedSides, err)...)
	res.out.State = domain.StateFailed
	if res.out.Execution == domain.ExecutionDenied {
		res.out.State = domain.StateDenied
	}
	return res.out
}

// explicitSides are the trace-scoped contexts of an explicit comparison:
// the Application's and each side's. render.timeout bounds the base stage
// from its start; the head stage gets its own timeout when it starts.
type explicitSides struct {
	app, base, head        context.Context
	cancelBase, cancelHead context.CancelFunc
}

func (r *run) openSides(ctx context.Context) explicitSides {
	s := explicitSides{app: ctx}
	s.base, s.cancelBase = context.WithTimeout(traceSide(ctx, "base", r.pair.Base, "explicit"), r.cfg.Timeout())
	s.head, s.cancelHead = context.WithCancel(traceSide(ctx, "head", r.pair.Head, "explicit"))
	return s
}

func (s explicitSides) cancel() {
	s.cancelHead()
	s.cancelBase()
}

// application compares one explicit Application on both revisions:
// selectExplicit (lookup → repository selection → prepareTarget) →
// renderPair (base: planSide → checkProject → renderSide; head: planSide →
// renderSide) → compareExplicit (parseSide for each side → compare). The
// project is checked once, before either render. render.timeout bounds each
// stage, as for a graph node: the lookup, the base side (plan, project
// check, render) and the head side (plan, render).
func (r *run) application(parent context.Context, id domain.ApplicationID, slots chan struct{}) (out domain.ApplicationResult) {
	ctx := domain.TraceWalk(domain.TraceApplication(parent, id), "explicit")
	res := &explicitResult{ctx: ctx, repository: r.cfg.Git.RepositoryURL, pair: r.pair, out: domain.ApplicationResult{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, State: domain.StateUnchanged, Execution: domain.ExecutionComplete, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{}, Inputs: domain.InputSummary{ValueFiles: []string{}, Parameters: []string{}, FileParameters: []string{}, Origin: "stored-application-spec"}}}
	defer func() {
		// A panic fails this Application only; resources compared
		// before it are incomplete and are not kept.
		if value := recover(); value != nil {
			res.out.Resources = []domain.ResourceDiff{}
			out = res.fail(domain.PanicFailure(value, "preview"), "")
		}
	}()
	t, selected, err := r.selectExplicit(ctx, id, &res.out)
	if err != nil {
		return res.fail(err, "")
	}
	if !selected {
		domain.TraceStart(ctx, "result", "application", "")(&domain.RenderSkipped{Reason: res.out.SkipReason})
		return res.out
	}
	sides := r.openSides(ctx)
	defer sides.cancel()
	base, head, side, err := r.renderPair(sides, t, slots, res)
	if err != nil {
		return res.fail(err, side)
	}
	return r.compareExplicit(sides, t, base, head, res)
}

// selectExplicit looks id up and records its project, destination and
// inputs on out. It reports false without an error when the Application
// does not reference the selected repository; out is then skipped.
func (r *run) selectExplicit(ctx context.Context, id domain.ApplicationID, out *domain.ApplicationResult) (preparedTarget, bool, error) {
	if ctx.Err() != nil {
		return preparedTarget{}, false, domain.Fail(domain.CodeCancelled, "render", cancelledReason, "rerun the preview", ctx.Err())
	}
	b := r.backends[id.Instance]
	if b == nil {
		return preparedTarget{}, false, domain.Fail(domain.CodeRoutingInvalid, "lookup", "configured instance has no renderer", "configure the instance renderer", nil)
	}
	stage, cancel := context.WithTimeout(ctx, r.cfg.Timeout())
	app, err := lookupApplication(stage, b, id)
	cancel()
	if err != nil {
		return preparedTarget{}, false, err
	}
	out.Project = app.Project()
	source := app.Source()
	out.Cluster, out.Namespace = destination(app)
	out.Inputs = inputSummary(source)
	out.Inputs.IgnoreDifferences = app.IgnoreRuleCount()
	if source == nil && !multipleSources(app) {
		return preparedTarget{}, false, domain.Fail(domain.CodeUnsupportedSource, "selection", "Application does not contain a supported single source", "select a single-source Git Helm Application", nil)
	}
	if !referencesRepository(app, r.cfg.Git.RepositoryURL, r.cfg.Git.Aliases()...) {
		out.State = domain.StateSkipped
		out.Execution = domain.ExecutionSkipped
		out.SkipReason = otherRepositorySkip
		return preparedTarget{}, false, nil
	}
	t, err := r.prepareTarget(id, app, true)
	return t, err == nil, err
}

// renderPair plans and renders both sides of t and returns their raw
// targets and partial errors, or the error and side that end the
// comparison. After the project check, the head side plans and renders
// alongside the base render when slots has a free slot; otherwise it runs
// after the base render. Errors are taken in the sequential order either
// way: a base render that fails outright discards (and cancels) the head
// side, as if it had not run. A failing head side first records a partial
// base render on res: it is a failing side too, whose issues stay and which
// takes part in the execution, also when the head side is skipped.
func (r *run) renderPair(sides explicitSides, t preparedTarget, slots chan struct{}, res *explicitResult) (base, head renderedSide, side string, err error) {
	pair := r.pair
	basePlan, err := r.planSide(sides.base, t, pair.Base)
	res.out.BaseSources = basePlan.sources
	if err != nil {
		return base, head, "base", err
	}
	if t.projectChecked() {
		// The check is part of the base stage but not of either side.
		deadline, _ := sides.base.Deadline()
		project, cancel := context.WithDeadline(sides.app, deadline)
		err = checkProject(project, t)
		cancel()
		if err != nil {
			return base, head, "", err
		}
	}
	var headRun headSide
	renderHead := func() {
		// The head stage starts when the head side does.
		stage, cancel := context.WithTimeout(sides.head, r.cfg.Timeout())
		defer cancel()
		headRun.plan, headRun.planErr = r.planSide(stage, t, pair.Head)
		if headRun.planErr == nil {
			headRun.raw, headRun.renderErr = renderSide(stage, t, headRun.plan)
		}
	}
	waitHead := alongside(slots, renderHead)
	base.raw, base.err = renderSide(sides.base, t, basePlan)
	if base.err != nil && !domain.IsRenderPartial(base.err) {
		if waitHead != nil {
			sides.cancelHead()
			waitHead()
		}
		return base, head, "base", base.err
	}
	if waitHead != nil {
		waitHead()
	} else {
		renderHead()
	}
	res.out.HeadSources = headRun.plan.sources
	head = renderedSide{raw: headRun.raw, err: headRun.renderErr}
	err = headRun.planErr
	if err == nil && head.err != nil && !domain.IsRenderPartial(head.err) {
		err = head.err
	}
	if err != nil {
		res.failedSides = markSideFailures(&res.out, failedSide{"base", res.repository, pair.Base, base.err})
		return base, head, "head", err
	}
	return base, head, "", nil
}

// compareExplicit records the partial sides of a rendered pair, parses
// both sides and compares their resources.
func (r *run) compareExplicit(sides explicitSides, t preparedTarget, base, head renderedSide, res *explicitResult) domain.ApplicationResult {
	pair := r.pair
	res.failedSides = markSideFailures(&res.out, failedSide{"base", res.repository, pair.Base, base.err}, failedSide{"head", res.repository, pair.Head, head.err})
	endParse := domain.TraceStart(sides.base, "parse", "target", "")
	baseTargets, err := r.parseSide(t, base.raw)
	res.out.BaseRepeatedResources = baseTargets.repeated
	endParse(err)
	if err != nil {
		return res.fail(err, "base")
	}
	endParse = domain.TraceStart(sides.head, "parse", "target", "")
	headTargets, err := r.parseSide(t, head.raw)
	res.out.HeadRepeatedResources = headTargets.repeated
	endParse(err)
	if err != nil {
		return res.fail(err, "head")
	}
	endDiff := domain.TraceStart(sides.app, "diff", "application", "")
	applyResourceDiff(&res.out, baseTargets.resources, headTargets.resources, base.err == nil, head.err == nil, r.redactor)
	endDiff(nil)
	if sides.app.Err() != nil {
		return res.fail(sides.app.Err(), "")
	}
	domain.TraceStart(sides.app, "result", "application", "")(nil)
	return res.out
}
