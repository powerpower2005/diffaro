package reporter

import (
	"sort"

	"diffaro/internal/domain"
)

// reportView holds the facts the human-readable formats derive from one
// redacted report. Encode builds it once; every encoder reads it instead of
// deriving the same facts again.
type reportView struct {
	report   domain.Report
	counts   reportCounts
	coverage coverage
	// ordered lists every Application by Argo CD instance, destination and
	// identity (appViewLess).
	ordered []*appView
	// review lists, in that order, the Applications that need review
	// (appNeedsReview); reviewGroups counts them per instance and
	// destination.
	review       []*appView
	reviewGroups []reviewGroup
	// reportIssues are the report's own issues; issues are the report and
	// Application issues (collectIssues). In both an issue repeated at base
	// and head is merged into one. errors groups issues by cause; failedApps
	// are the Applications the causes name.
	reportIssues []domain.Issue
	issues       []domain.Issue
	errors       []*errorGroup
	failedApps   map[string]bool
	// verdict explains the status and exit code (verdictFrom).
	verdict verdict
	// roots names the starting roots (startRoots).
	roots []string
	// images are the observed container image changes in report order.
	images []reportImage
	// inputs merges the changed render inputs the Applications read
	// (inputGroups); inputReaders counts the Applications that read them.
	inputs       []inputGroup
	inputReaders int
	// prApps lists the Applications to review first, then the others; PR
	// Markdown follows this order. prGroups sorts them by how the PR change
	// list shows them, and prContext orders them for the render context:
	// those with a change section first.
	prApps    []*appView
	prGroups  prGroups
	prContext []*appView
}

// appView is one Application with the facts the formats show about it,
// derived once per report.
type appView struct {
	domain.ApplicationResult
	// id is ID.String().
	id          string
	destination reviewDestination // destinationForReview
	namespace   string            // reviewNamespace
	action      string            // applicationReviewAction
	needsReview bool              // appNeedsReview
	// issues are the Application's issues, an issue repeated at base and
	// head merged into one.
	issues []domain.Issue
	// resources counts the resources by state.
	resources map[domain.State]int
}

func newAppView(a domain.ApplicationResult) *appView {
	v := &appView{ApplicationResult: a, id: a.ID.String(), destination: destinationForReview(a), namespace: reviewNamespace(a), action: applicationReviewAction(a), needsReview: appNeedsReview(a), resources: map[domain.State]int{}}
	if len(a.Issues) > 0 {
		v.issues = domain.GroupIssues(a.Issues)
	}
	for _, res := range a.Resources {
		v.resources[res.State]++
	}
	return v
}

func newReportView(r domain.Report) *reportView {
	v := &reportView{report: r, counts: countReport(r), ordered: orderedApplications(r)}
	v.coverage = coverageOf(r, v.counts)
	v.review, v.reviewGroups = navigation(v.ordered)
	v.reportIssues = domain.GroupIssues(r.Issues)
	v.issues = collectIssues(r)
	v.errors = errorGroups(r, v.issues)
	v.failedApps = failedApps(v.errors)
	v.verdict = verdictFrom(r, v.counts)
	v.roots = startRoots(r)
	v.images = reportImages(r)
	v.inputs, v.inputReaders = inputGroups(v.ordered)
	v.prApps = append([]*appView(nil), v.review...)
	for _, app := range v.ordered {
		if !app.needsReview {
			v.prApps = append(v.prApps, app)
		}
	}
	v.prGroups = groupApplications(v.prApps)
	v.prContext = append([]*appView(nil), v.prGroups.sections...)
	sectioned := map[domain.ApplicationID]bool{}
	for _, app := range v.prGroups.sections {
		sectioned[app.ID] = true
	}
	for _, app := range v.prApps {
		if !sectioned[app.ID] {
			v.prContext = append(v.prContext, app)
		}
	}
	return v
}

// startRoots names the root of every Application, or the Application itself
// when it has none, once each and sorted.
func startRoots(r domain.Report) []string {
	roots := map[string]bool{}
	for _, app := range r.Applications {
		root := app.Root
		if root.Name == "" {
			root = app.ID
		}
		if root.Name != "" {
			roots[root.String()] = true
		}
	}
	names := make([]string, 0, len(roots))
	for root := range roots {
		names = append(names, root)
	}
	sort.Strings(names)
	return names
}

// coverage holds the facts that show the analysis did not cover everything.
// The formats deliberately combine them differently:
//
//   - full Markdown warns once when any of them holds (markdownIncomplete);
//   - the terminal warns when Applications failed or were skipped, resources
//     were compared incompletely or the status is incomplete
//     (terminalIncomplete); incomplete graph discovery has its own line, and
//     unknown Applications are counted but do not warn;
//   - PR Markdown has a line each for skipped Applications, an incomplete
//     status without failure causes and incomplete graph discovery; failed
//     Applications are in Needs attention.
type coverage struct {
	failed, skipped     int // failed or denied, and skipped Applications
	incompleteResources int // resources with resourceComparisonIncomplete
	unknownApps         int // Applications whose state is unknown
	// graphIncomplete: base or head graph discovery is incomplete.
	graphIncomplete bool
	// statusIncomplete: the status is neither success nor policy violation.
	statusIncomplete bool
}

func coverageOf(r domain.Report, c reportCounts) coverage {
	return coverage{
		failed: c.failed, skipped: c.skipped, incompleteResources: c.incomplete, unknownApps: c.apps[domain.StateUnknown],
		graphIncomplete:  r.Graph != nil && (!r.Graph.BaseComplete || !r.Graph.HeadComplete),
		statusIncomplete: r.Status != domain.StatusSuccess && r.Status != domain.StatusPolicyViolation,
	}
}

// markdownIncomplete: full Markdown warns that analysis coverage is
// incomplete.
func (c coverage) markdownIncomplete() bool {
	return c.failed > 0 || c.skipped > 0 || c.incompleteResources > 0 || c.unknownApps > 0 || c.graphIncomplete || c.statusIncomplete
}

// terminalIncomplete: the terminal warns that analysis is incomplete.
func (c coverage) terminalIncomplete() bool {
	return c.skipped > 0 || c.failed > 0 || c.incompleteResources > 0 || c.statusIncomplete
}

// The formats deliberately use different definitions of an Application with
// changes and of an incomplete resource; each has its own name:
//
//   - appNeedsReview: anything other than a completely compared, unchanged
//     Application without issues, skip reason, spec, input or pruning changes
//     and with every resource unchanged and completely compared. It selects
//     the review index and the terminal details.
//   - appHasChanges: added or removed, desired spec changes or a changed
//     resource. It counts the Applications of the PR headline.
//   - a changed Application state (State.IsChange): the verdict's "Changes"
//     count (reportCounts.appsStateChanged).
//   - resourceComparisonIncomplete: the comparison was incomplete. It is
//     counted as an incomplete resource comparison in every format.
//   - resourceNotFullyCompared: unknown state, incomplete comparison or fields
//     that differ between renders. It gives a resource and its Application a
//     place in the PR change list.

// appNeedsReview reports an Application a reviewer has to look at. Its
// negation is a completely compared Application without any change.
func appNeedsReview(a domain.ApplicationResult) bool {
	if a.State != domain.StateUnchanged || a.Execution != domain.ExecutionComplete || len(a.Issues) > 0 || a.SkipReason != "" || len(a.SpecChanges) > 0 || len(a.InputChanges) > 0 || a.Pruning != nil {
		return true
	}
	for _, resource := range a.Resources {
		if resource.State != domain.StateUnchanged || resourceComparisonIncomplete(resource) {
			return true
		}
	}
	return false
}

// appHasChanges reports an Application with observed target or Application
// changes.
func appHasChanges(app domain.ApplicationResult) bool {
	if app.State == domain.StateAdded || app.State == domain.StateRemoved || len(app.SpecChanges) > 0 {
		return true
	}
	for _, res := range app.Resources {
		if res.State.IsChange() {
			return true
		}
	}
	return false
}

// resourceComparisonIncomplete reports a resource whose comparison is
// incomplete.
func resourceComparisonIncomplete(res domain.ResourceDiff) bool {
	return res.Comparison == domain.ComparisonIncomplete
}

// resourceNotFullyCompared reports a resource whose targets could not be
// compared fully: its state is unknown, its comparison is incomplete or some
// fields were not compared.
func resourceNotFullyCompared(res domain.ResourceDiff) bool {
	return res.State == domain.StateUnknown || resourceComparisonIncomplete(res) || len(res.UnknownPaths) > 0
}

// notCompared reports an Application whose targets were not compared.
func notCompared(app domain.ApplicationResult) bool {
	return app.Execution.FailedOrDenied() || app.Execution == domain.ExecutionSkipped
}

// reportCounts summarizes Application and resource outcomes shared by every
// human-readable format, for a report or one review group.
type reportCounts struct {
	applications    int
	apps, resources map[domain.State]int // by state
	failed          int                  // failed or denied Applications
	skipped         int
	incomplete      int // resources with resourceComparisonIncomplete
	// appsWithChanges counts appHasChanges; appsStateChanged counts
	// Applications whose state is added, changed or removed.
	appsWithChanges  int
	appsStateChanged int
	compared         int // Applications that are not notCompared
}

func newCounts() reportCounts {
	return reportCounts{apps: map[domain.State]int{}, resources: map[domain.State]int{}}
}

func countReport(r domain.Report) reportCounts {
	c := newCounts()
	for _, app := range r.Applications {
		c.add(app)
	}
	return c
}

func (c *reportCounts) add(app domain.ApplicationResult) {
	c.applications++
	c.apps[app.State]++
	if app.Execution.FailedOrDenied() {
		c.failed++
	}
	if app.Execution == domain.ExecutionSkipped {
		c.skipped++
	}
	if appHasChanges(app) {
		c.appsWithChanges++
	}
	if app.State.IsChange() {
		c.appsStateChanged++
	}
	if !notCompared(app) {
		c.compared++
	}
	for _, resource := range app.Resources {
		c.resources[resource.State]++
		if resourceComparisonIncomplete(resource) {
			c.incomplete++
		}
	}
}

const maxReviewGroups = 8
const maxReviewApps = 12

// Presentation projections only.
type reviewDestination struct{ instance, base, head string }
type reviewGroup struct {
	destination reviewDestination
	counts      reportCounts
}

func destinationForReview(a domain.ApplicationResult) reviewDestination {
	d := reviewDestination{instance: a.ID.Instance, base: a.Cluster, head: a.Cluster}
	if a.BaseTarget != nil {
		d.base = a.BaseTarget.Cluster
	}
	if a.HeadTarget != nil {
		d.head = a.HeadTarget.Cluster
	}
	if a.State == domain.StateAdded && a.BaseTarget == nil {
		d.base = d.head
	}
	if a.State == domain.StateRemoved && a.HeadTarget == nil {
		d.head = d.base
	}
	return d
}

func knownDestination(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (d reviewDestination) label() string {
	if d.base == d.head {
		return knownDestination(d.head)
	}
	return knownDestination(d.base) + " -> " + knownDestination(d.head) + " (base -> head)"
}

func (d reviewDestination) less(o reviewDestination) bool {
	if d.instance != o.instance {
		return d.instance < o.instance
	}
	if d.base != o.base {
		return d.base < o.base
	}
	return d.head < o.head
}

// appViewLess orders Applications by destination, then identity.
func appViewLess(a, b *appView) bool {
	if a.destination != b.destination {
		return a.destination.less(b.destination)
	}
	return a.id < b.id
}

func orderedApplications(r domain.Report) []*appView {
	apps := make([]*appView, len(r.Applications))
	for i, app := range r.Applications {
		apps[i] = newAppView(app)
	}
	sort.SliceStable(apps, func(i, j int) bool { return appViewLess(apps[i], apps[j]) })
	return apps
}

// navigation selects, in the given order, the Applications that need review
// and counts them per instance and destination.
func navigation(ordered []*appView) ([]*appView, []reviewGroup) {
	var apps []*appView
	index := map[reviewDestination]int{}
	var groups []reviewGroup
	for _, a := range ordered {
		if !a.needsReview {
			continue
		}
		apps = append(apps, a)
		key := a.destination
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, reviewGroup{destination: key, counts: newCounts()})
		}
		groups[i].counts.add(a.ApplicationResult)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].destination.less(groups[j].destination) })
	return apps, groups
}

// prGroups sorts Applications by how the change list shows them: a section
// each, or a name on one of three lines.
type prGroups struct {
	sections, unknown, inputsOnly, unchanged []*appView
}

// groupApplications gives a section to an Application with changes,
// resources that could not be compared fully, pruning candidates or
// placeholder input, and names the others by outcome.
func groupApplications(apps []*appView) prGroups {
	var g prGroups
	for _, app := range apps {
		section := appHasChanges(app.ApplicationResult) || app.Pruning != nil || app.PlaceholderInput != nil
		for _, res := range app.Resources {
			section = section || resourceNotFullyCompared(res)
		}
		switch {
		case section:
			g.sections = append(g.sections, app)
		case notCompared(app.ApplicationResult):
			g.unknown = append(g.unknown, app)
		case len(app.InputChanges) > 0:
			g.inputsOnly = append(g.inputsOnly, app)
		default:
			g.unchanged = append(g.unchanged, app)
		}
	}
	return g
}

// collectIssues returns report and Application issues for human-readable
// sections, merging an issue repeated at base and head.
func collectIssues(report domain.Report) []domain.Issue {
	issues := append([]domain.Issue(nil), report.Issues...)
	for _, app := range report.Applications {
		issues = append(issues, app.Issues...)
	}
	return domain.GroupIssues(issues)
}
