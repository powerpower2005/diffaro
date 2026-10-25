// Package preview orchestrates a comparison: it selects target Applications,
// renders base and head through the configured domain.Renderer per Argo CD
// instance, parses and diffs manifests and assembles the domain.Report with
// explicit failed, denied and skipped states.
//
// Compare (comparison.go) selects the mode and completes the comparison in
// one scope, reusing discovery's chart readings for input evidence. The mode
// entry points are Runner.Run (explicit Applications, explicit.go),
// Runner.RunRecursive (App-of-Apps, recursive.go) and Runner.RunDiscovered
// (repository discovery, discovered_run.go), plus Doctor. Each entry point
// builds its own run (runner.go) holding the per-comparison state: resolved
// backend capabilities (backends.go), the source revision cache and, for
// graphs, the render cache, cluster scopes, descendant selection and root
// errors.
//
// Every comparison mode renders an Application through the same stages, each
// with typed input and output:
//
//	prepareTarget  Application → preparedTarget            support, destination, backend (prepare.go)
//	planSide       preparedTarget, revision → plannedSide  multi-source plan and capability (render.go)
//	checkProject   preparedTarget → error                  AppProject (render.go)
//	renderSide     preparedTarget, plannedSide → []string  backend render (render.go)
//	parseSide      preparedTarget, []string → parsedSide   resources and repeats (render.go)
//
// Explicit Applications run them once per side after lookup and repository
// selection, checking the project once before either render, in a rolling
// window of render.maxConcurrency slots (the head side renders alongside the
// base side while a slot is free); graph nodes
// (run.expand) run them per walked revision. Graph results come from
// compareNode (compare.go), a pure function of the base and head nodes and
// their presence. retain.go bounds retained result bytes and defines the
// useful result that every mode uses to tell partial failure from failure; report.go holds report notes,
// ApplyRunIssues and finishReport, which every mode and Doctor end with.
//
// Passes around a run: SelectGenerated and VerifySelections (ApplicationSet
// selection), then Runner.CompleteComparison (MaskRepeatedValues,
// Runner.AttachInputChanges and AnnotateFailureOrigins with the limitation
// order), then Finalize (ExcludePreexistingFailures (opt-in), policies and
// backend versions). finalize.go holds these post-run report steps
// (CompleteComparison, which diff, doctor --render and the demo share,
// Finalize, RevisionFailureReport and ApplyBackendSetupIssues) so commands
// make no report decisions.
package preview

import (
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/profile"
	"diffaro/internal/security"
)

// Runner holds the configuration of comparisons. Each entry point (Run,
// RunRecursive, RunDiscovered) builds its own run, so a Runner keeps no state
// between comparisons.
type Runner struct {
	Config         profile.Config
	Renderers      map[string]domain.Renderer
	Redactor       security.Redactor
	maxResultBytes int // Tests can lower the production retained-result budget.
	// ChangedPaths enables opt-in changed-path selection for discovery runs.
	ChangedPaths *ChangedPaths
	// BackendLimitations rewords the backend-dependent recursive
	// limitations; empty fields keep the wording for the profile's backends.
	BackendLimitations BackendLimitations
}

// run is the state of one comparison. Every field is set when the run is
// built; no stage depends on state left by an earlier comparison.
type run struct {
	cfg            profile.Config
	redactor       security.Redactor
	maxResultBytes int
	pair           domain.RevisionPair
	// backends holds each instance's renderer with its optional
	// capabilities resolved once.
	backends map[string]*backend
	// sourceRevisions freezes each non-PR source reference once for the
	// whole comparison, including graph probes and both walks.
	sourceRevisions *sourceRevisionCache

	// The remaining fields are used by graph comparisons only.

	// cache reuses renders of identical inputs across candidate probes and
	// both walks.
	cache *expansionCache
	// scopes completes resource scopes from other Applications with the
	// same destination cluster; nil outside repository discovery.
	scopes clusterScopes
	// descendants skips children proven unaffected; nil unless
	// changed-path selection is enabled.
	descendants *descendantSelector
	// rootErrors holds root lookup and root selection failures. A root with
	// an error is reported, not rendered.
	rootErrors map[domain.ApplicationID]error
	// limitations words the backend-dependent recursive limitations.
	limitations BackendLimitations
}

func (r Runner) newRun(pair domain.RevisionPair) *run {
	return &run{cfg: r.Config, redactor: r.Redactor, maxResultBytes: r.maxResultBytes, pair: pair, backends: resolveBackends(r.Renderers), sourceRevisions: newSourceRevisionCache()}
}

// newGraphRun builds the state of a graph comparison. scopes and descendants
// are only set by repository discovery.
func (r Runner) newGraphRun(pair domain.RevisionPair, scopes clusterScopes, descendants *descendantSelector) *run {
	x := r.newRun(pair)
	x.scopes, x.descendants = scopes, descendants
	x.rootErrors = map[domain.ApplicationID]error{}
	x.limitations = backendLimitations(r.BackendLimitations, x.cfg.Instances)
	x.cache = newExpansionCache(domain.RetainedByteLimit, x.cfg.Render.MaxConcurrency)
	// A child that renders at its own source commit renders identically on
	// both revisions, so base and head share one render; roots keep
	// revision-specific renders.
	x.cache.independent = func(app domain.Application, root bool) bool {
		return !root && x.rendersAtOwnCommit(app)
	}
	return x
}

// rendersAtOwnCommit reports whether app renders at its own resolved source
// commit instead of the walked revision: a single source from another
// repository (run.expand). The render cache relies on the same rule.
func (r *run) rendersAtOwnCommit(app domain.Application) bool {
	return !multipleSources(app) && !r.cfg.Git.Matches(domain.String(app.Source(), "repoURL"))
}

// backendLimitations is texts with the basic analysis limitation worded for
// local Helm when any instance uses it and texts leaves it empty.
func backendLimitations(texts BackendLimitations, instances []profile.Instance) BackendLimitations {
	for _, instance := range instances {
		if instance.LocalHelm && texts.BasicAnalysis == "" {
			texts.BasicAnalysis = localHelmBasicAnalysisLimitation
		}
	}
	return texts
}

func (r *run) limits() manifest.Limits {
	return manifest.Limits{Bytes: r.cfg.Render.MaxResponseBytes, Documents: r.cfg.Render.MaxDocuments}
}

// bounded runs fn for 0..n-1 with at most limit concurrent calls. A panic
// in fn is raised again in the caller once every call has returned, with
// the panicking goroutine's stack.
func bounded(n, limit int, fn func(int)) {
	slots := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	var panicked panicSlot
	for i := range n {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			defer panicked.capture()
			fn(i)
		}()
	}
	wg.Wait()
	panicked.raise()
}

// panicSlot carries the first panic of worker goroutines to their owner.
type panicSlot struct {
	once    sync.Once
	failure *domain.Failure
}

// capture, deferred in a worker, records its panic instead of crashing.
func (p *panicSlot) capture() {
	if value := recover(); value != nil {
		failure := domain.PanicFailure(value, "preview")
		p.once.Do(func() { p.failure = failure })
	}
}

// raise panics in the owner with the captured failure, if any. Call it
// after every worker has returned.
func (p *panicSlot) raise() {
	if p.failure != nil {
		panic(p.failure)
	}
}

// guarded returns fn's results, or an internal_error failure of stage when
// fn panics, so a panic fails only that step.
func guarded[T any](stage string, fn func() (T, error)) (value T, err error) {
	defer domain.RecoverAs(&err, stage)
	return fn()
}
