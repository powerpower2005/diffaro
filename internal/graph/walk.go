package graph

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
)

// MaxFaults bounds the faults a Snapshot retains, independently of an
// adversarial number of edges. A Snapshot holding MaxFaults faults may have
// dropped others.
const MaxFaults = 128

type Expansion struct {
	RepeatedResources                  []domain.ResourceID
	Sources                            []domain.SourceSummary
	Repository, Revision, RevisionRule string
	// RequestedRevision is the reference Revision was resolved from at run
	// time, when RevisionRule is resolved-source-reference.
	RequestedRevision string
	Resources         map[string]manifest.Resource
	// ResourceBytes is ResourceSize(Resources), which a walk charges to its
	// retained-byte budget. Zero means not measured yet; an Expand that
	// returns one expansion to several walks measures it once.
	ResourceBytes int `json:"-"`
}

// ResourceSize returns the JSON size of resources, or -1 when they cannot be
// encoded.
func ResourceSize(resources map[string]manifest.Resource) int {
	b, err := json.Marshal(resources)
	if err != nil {
		return -1
	}
	return len(b)
}

type Node struct {
	Application domain.Application
	Root        domain.ApplicationID
	Path        []domain.ApplicationID
	Expansion
	Rendered   bool
	Discovered bool   // Direct child edges are known, independently of descendants.
	Ambiguous  bool   // The node depends on conflicting desired specs.
	SkipReason string // Resource rendering and descendant discovery were omitted.
	Err        error
	// Placeholder is set when this node, or an ancestor, was rendered from
	// a desired spec holding domain.RenderUnstable.
	Placeholder *domain.PlaceholderInput
}
type Fault struct {
	ID                   domain.ApplicationID
	Repository, Revision string
	Err                  error
}
type Snapshot struct {
	Nodes  map[domain.ApplicationID]*Node
	Edges  []domain.GraphEdge
	Faults []Fault
	// Skipped maps each child that Skip omitted and no other path visited to
	// its least parent. Absence of a skipped child is not evidence of removal.
	Skipped  map[domain.ApplicationID]domain.ApplicationID
	Complete bool
}
type Walker struct {
	MaxDepth, MaxApplications int
	MaxBytes                  int // zero selects domain.RetainedByteLimit
	Expand                    func(context.Context, domain.Application, bool) (Expansion, error)
	Route                     func(domain.Application, domain.Application) (string, string, error)
	// Prefetch, when set, receives a node's routable children before they are
	// visited in order, so their expansions can start concurrently. Expand
	// must then share in-flight and finished expansions; visit order and
	// results stay those of the sequential walk.
	Prefetch func(context.Context, []domain.Application)
	// Skip, when set, reports that a rendered child is proven identical on
	// both revisions. Its edge is kept, but it is neither visited nor
	// prefetched. Skip must decide identically in the base and head walks;
	// Snapshot.Skipped lets comparison detect a one-sided decision.
	Skip func(ctx context.Context, parent domain.Application, root bool, child domain.Application) bool
}

// placeholderSkip turns a render failure of an Application rendered from a
// placeholder input into a skip: the failure may come from the placeholder
// rather than the chart. Other failures, such as denied access or
// a timeout, are kept.
func placeholderSkip(err error) error {
	var failure *domain.Failure
	switch {
	case domain.IsRenderPartial(err):
		return &domain.RenderSkipped{Reason: placeholderSkipReason + ": some templates failed", Cause: err}
	case errors.As(err, &failure) && failure.Issue.Stage == "render":
		return &domain.RenderSkipped{Reason: placeholderSkipReason + ": " + failure.Issue.Reason, Cause: err}
	}
	return err
}

const placeholderSkipReason = "rendered from a placeholder for spec fields that differ between two renders of identical inputs, and the render failed, possibly because of the placeholder"

// child returns the routed child Application a rendered resource declares,
// or a discovery failure. ok is false for resources that are not children.
func (w Walker) child(parent domain.Application, resource manifest.Resource) (child domain.Application, rule string, ok bool, err error) {
	if resource.ID.Group != "argoproj.io" {
		return child, "", false, nil
	}
	if resource.ID.Kind == "ApplicationSet" {
		return child, "", true, domain.Fail(domain.CodeUnsupportedApplicationSet, "discovery", "an ApplicationSet cannot be expanded by this backend", "use a verified generated-Application adapter before relying on complete graph coverage", nil)
	}
	if resource.ID.Kind != "Application" {
		return child, "", false, nil
	}
	spec, valid := resource.Object["spec"].(map[string]any)
	if !valid {
		return child, "", true, domain.Fail(domain.CodeChildSpecInvalid, "discovery", "rendered child Application has no valid spec", "provide a complete child Application spec", nil)
	}
	child = domain.Application{ID: domain.ApplicationID{Namespace: resource.ID.Namespace, Name: resource.ID.Name}, Spec: spec}
	if !validDNSName(child.ID.Name, 253, true) || !validDNSName(child.ID.Namespace, 63, false) {
		return child, "", true, domain.Fail(domain.CodeChildIdentityInvalid, "discovery", "child Application name or namespace is not a valid Kubernetes identity", "use DNS names for Application names and namespaces", nil)
	}
	instance, rule, err := w.Route(parent, child)
	if err != nil {
		return child, "", true, err
	}
	child.ID.Instance = instance
	return child, rule, true, nil
}

// Walk visits roots in identity order, depth first, then applies the
// whole-graph checks that a single path cannot decide: descendants of
// conflicting specs and depth through shared subtrees.
func (w Walker) Walk(ctx context.Context, roots []domain.Application) Snapshot {
	t := newWalk(ctx, w)
	ordered := append([]domain.Application{}, roots...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID.String() < ordered[j].ID.String() })
	for _, root := range ordered {
		t.visit(root, root.ID, nil, true)
	}
	for id := range t.s.Skipped {
		if t.s.Nodes[id] != nil {
			delete(t.s.Skipped, id)
		}
	}
	adjacent, indegree := t.adjacency()
	t.propagateConflicts(adjacent)
	t.checkSharedDepth(adjacent, indegree)
	sort.Slice(t.s.Edges, func(i, j int) bool {
		a, b := t.s.Edges[i], t.s.Edges[j]
		if a.Parent != b.Parent {
			return a.Parent.String() < b.Parent.String()
		}
		return a.Child.String() < b.Child.String()
	})
	return t.s
}

// walk is the state of one Walk call.
type walk struct {
	Walker
	ctx context.Context
	s   Snapshot
	// active holds the Applications on the current depth-first path.
	active map[domain.ApplicationID]bool
	// conflicts holds Applications whose specs conflict, or descend from one.
	conflicts                   map[domain.ApplicationID]bool
	budget, used, observedBytes int
}

func newWalk(ctx context.Context, w Walker) *walk {
	budget := w.MaxBytes
	if budget <= 0 {
		budget = domain.RetainedByteLimit
	}
	return &walk{
		Walker: w, ctx: ctx, budget: budget,
		s:      Snapshot{Nodes: map[domain.ApplicationID]*Node{}, Edges: []domain.GraphEdge{}, Faults: []Fault{}, Skipped: map[domain.ApplicationID]domain.ApplicationID{}, Complete: true},
		active: map[domain.ApplicationID]bool{}, conflicts: map[domain.ApplicationID]bool{},
	}
}

// reserve charges v's JSON size to the retained-byte budget and reports
// whether it fits.
func (t *walk) reserve(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return t.reserveBytes(-1)
	}
	return t.reserveBytes(len(b))
}

// reserveResources charges n's rendered resources, measured by Expand
// when it has set ResourceBytes.
func (t *walk) reserveResources(n *Node) bool {
	size := n.ResourceBytes
	if size == 0 {
		size = ResourceSize(n.Resources)
	}
	return t.reserveBytes(size)
}

// reserveBytes charges size bytes, or fails for an unencodable value (-1).
func (t *walk) reserveBytes(size int) bool {
	t.observedBytes = t.used + max(size, 0)
	if size < 0 || size > t.budget-t.used {
		return false
	}
	t.used += size
	return true
}

// fault marks the snapshot incomplete and retains at most MaxFaults faults.
func (t *walk) fault(id domain.ApplicationID, exp Expansion, err error) {
	t.s.Complete = false
	if len(t.s.Faults) < MaxFaults {
		t.s.Faults = append(t.s.Faults, Fault{id, exp.Repository, exp.Revision, err})
	}
}

func (t *walk) limit(subject string) error {
	return domain.LimitError(domain.CodeGraphLimitExceeded, "graph", subject, "revision graph retained-byte budget", int64(t.observedBytes), int64(t.budget), "bytes", true, 0)
}

// visit adds app below ancestors, renders it and visits its children.
func (t *walk) visit(app domain.Application, root domain.ApplicationID, ancestors []domain.ApplicationID, isRoot bool) {
	if !t.admit(app, ancestors) {
		return
	}
	n := t.addNode(app, root, ancestors)
	t.active[app.ID] = true
	defer delete(t.active, app.ID)
	if !t.expand(n, isRoot) {
		return
	}
	children := t.children(n)
	t.skipUnchanged(n, isRoot, children)
	t.prefetch(n, children)
	t.link(n, root, children)
}

// admit reports whether app becomes a new node. It faults on cancellation,
// the depth limit, a cycle, the Application limit and the byte budget, and
// marks a known node ambiguous when app gives it a different spec.
func (t *walk) admit(app domain.Application, ancestors []domain.ApplicationID) bool {
	origin := Expansion{Repository: domain.String(app.Source(), "repoURL"), Revision: domain.String(app.Source(), "targetRevision")}
	if len(ancestors) > 0 {
		origin = t.s.Nodes[ancestors[len(ancestors)-1]].Expansion
	}
	if t.ctx.Err() != nil {
		t.fault(app.ID, origin, t.ctx.Err())
		return false
	}
	if len(ancestors) > t.MaxDepth {
		t.fault(app.ID, origin, domain.LimitError(domain.CodeGraphLimitExceeded, "graph", "Application graph depth (root=0)", "render.maxDepth", int64(len(ancestors)), int64(t.MaxDepth), "levels", true, domain.MaxRenderDepth))
		return false
	}
	if t.active[app.ID] {
		t.fault(app.ID, t.s.Nodes[app.ID].Expansion, domain.Fail(domain.CodeGraphCycle, "graph", "Application identity repeats on the active graph path: "+cyclePath(ancestors, app.ID), "remove the cycle from the Application graph; the last Application before the repeated one defines it again", nil))
		return false
	}
	if prior := t.s.Nodes[app.ID]; prior != nil {
		if !reflect.DeepEqual(prior.Application.Spec, app.Spec) {
			err := domain.Fail(domain.CodeGraphSpecConflict, "graph", "multiple paths define different specs for the same Application identity", "give the Application a single unambiguous desired spec", nil)
			prior.Err, prior.Rendered, prior.Resources = err, false, nil
			prior.SkipReason = ""
			prior.Discovered = false
			prior.Ambiguous = true
			t.conflicts[app.ID] = true
			t.fault(app.ID, prior.Expansion, err)
		}
		return false
	}
	if len(t.s.Nodes) >= t.MaxApplications {
		t.fault(app.ID, origin, domain.LimitError(domain.CodeGraphLimitExceeded, "graph", "unique Application graph nodes", "render.maxApplications", int64(len(t.s.Nodes)+1), int64(t.MaxApplications), "Applications", true, domain.MaxRenderApplications))
		return false
	}
	if !t.reserve(app.Spec) {
		t.fault(app.ID, origin, t.limit("stored Application specs"))
		return false
	}
	return true
}

// cyclePath names the cycle that id closes on the active path ancestors,
// from its first occurrence to the repeat, by identities only.
func cyclePath(ancestors []domain.ApplicationID, id domain.ApplicationID) string {
	start := slices.Index(ancestors, id)
	if start < 0 {
		start = 0
	}
	var names []string
	for _, a := range append(slices.Clone(ancestors[start:]), id) {
		names = append(names, a.String())
	}
	return strings.Join(names, " -> ")
}

// addNode records app's node with its path and inherited or own placeholder.
func (t *walk) addNode(app domain.Application, root domain.ApplicationID, ancestors []domain.ApplicationID) *Node {
	path := append(append([]domain.ApplicationID{}, ancestors...), app.ID)
	n := &Node{Application: app, Root: root, Path: path}
	if len(ancestors) > 0 {
		n.Placeholder = t.s.Nodes[ancestors[len(ancestors)-1]].Placeholder
	}
	if paths := domain.UnstablePaths("/spec", app.Spec); n.Placeholder == nil && len(paths) > 0 {
		n.Placeholder = &domain.PlaceholderInput{Application: app.ID, Paths: paths}
	}
	t.s.Nodes[app.ID] = n
	return n
}

// expand renders n and reports whether its children can be discovered.
// Skips, failures and byte-budget overruns are recorded on n.
func (t *walk) expand(n *Node, isRoot bool) bool {
	app := n.Application
	n.Expansion, n.Err = t.Expand(t.ctx, app, isRoot)
	if n.Placeholder != nil {
		n.Err = placeholderSkip(n.Err)
	}
	var skipped *domain.RenderSkipped
	if errors.As(n.Err, &skipped) {
		n.SkipReason, n.Err, n.Resources = skipped.Reason, nil, nil
		t.s.Complete = false
		return false
	}
	if n.Err != nil {
		if domain.IsRenderPartial(n.Err) {
			if t.reserveResources(n) {
				n.Rendered = true
			} else {
				n.Err = t.limit("stored Application specs and rendered resources")
				n.Resources = nil
			}
		} else {
			n.Resources = nil
		}
		t.fault(app.ID, n.Expansion, n.Err)
		return false
	}
	if !t.reserveResources(n) {
		n.Err = t.limit("stored Application specs and rendered resources")
		n.Resources = nil
		t.fault(app.ID, n.Expansion, n.Err)
		return false
	}
	n.Rendered = true
	n.Discovered = true
	return true
}

// declaredChild is a child Application declared by one rendered resource,
// or the failure to resolve it.
type declaredChild struct {
	app       domain.Application
	rule      string
	err       error
	unchanged bool // Skip proved it identical on both revisions.
}

// children resolves, once per resource and in resource key order, the
// child Applications n's rendered resources declare.
func (t *walk) children(n *Node) []declaredChild {
	var children []declaredChild
	for _, key := range slices.Sorted(maps.Keys(n.Resources)) {
		if child, rule, ok, err := t.child(n.Application, n.Resources[key]); ok {
			children = append(children, declaredChild{app: child, rule: rule, err: err})
		}
	}
	return children
}

// skipUnchanged asks Skip about every valid child. A placeholder spec does
// not prove equal inputs, so such children are always visited.
func (t *walk) skipUnchanged(n *Node, isRoot bool, children []declaredChild) {
	if t.Skip == nil {
		return
	}
	for i := range children {
		if c := &children[i]; c.err == nil && n.Placeholder == nil && domain.UnstablePaths("", c.app.Spec) == nil {
			c.unchanged = t.Skip(t.ctx, n.Application, isRoot, c.app)
		}
	}
}

// prefetch hands Prefetch the children that link will visit for the first
// time, when they are within the depth limit.
func (t *walk) prefetch(n *Node, children []declaredChild) {
	if t.Prefetch == nil || len(n.Path) > t.MaxDepth {
		return
	}
	var ahead []domain.Application
	for _, c := range children {
		if c.err == nil && !c.unchanged && t.s.Nodes[c.app.ID] == nil {
			ahead = append(ahead, c.app)
		}
	}
	if len(ahead) > 0 {
		t.Prefetch(t.ctx, ahead)
	}
}

// link records an edge to every resolved child and visits those not proven
// unchanged. Resolution failures and the byte budget end n's discovery.
func (t *walk) link(n *Node, root domain.ApplicationID, children []declaredChild) {
	app := n.Application
	for _, c := range children {
		if c.err != nil {
			n.Discovered = false
			t.fault(app.ID, n.Expansion, c.err)
			continue
		}
		edge := domain.GraphEdge{Parent: app.ID, Child: c.app.ID, Repository: n.Repository, Revision: n.Revision, Method: "rendered-application", Routing: c.rule}
		if !t.reserve(edge) {
			n.Discovered = false
			t.fault(app.ID, n.Expansion, t.limit("stored Application specs, resources and graph edges"))
			break
		}
		t.s.Edges = append(t.s.Edges, edge)
		if !c.unchanged {
			t.visit(c.app, root, n.Path, false)
		} else if prior, ok := t.s.Skipped[c.app.ID]; !ok || app.ID.String() < prior.String() {
			t.s.Skipped[c.app.ID] = app.ID
		}
	}
}

// adjacency returns the children and in-degree of every node over edges
// between visited nodes.
func (t *walk) adjacency() (adjacent map[domain.ApplicationID][]domain.ApplicationID, indegree map[domain.ApplicationID]int) {
	indegree, adjacent = map[domain.ApplicationID]int{}, map[domain.ApplicationID][]domain.ApplicationID{}
	for id := range t.s.Nodes {
		indegree[id] = 0
	}
	for _, edge := range t.s.Edges {
		if t.s.Nodes[edge.Parent] != nil && t.s.Nodes[edge.Child] != nil {
			indegree[edge.Child]++
			adjacent[edge.Parent] = append(adjacent[edge.Parent], edge.Child)
		}
	}
	return adjacent, indegree
}

// propagateConflicts fails every descendant of a conflicting node.
// Descendants obtained from a conflicting parent's arbitrary first spec are
// not trustworthy comparison inputs, including when another path shares them.
func (t *walk) propagateConflicts(adjacent map[domain.ApplicationID][]domain.ApplicationID) {
	invalid := []domain.ApplicationID{}
	for id := range t.conflicts {
		invalid = append(invalid, id)
	}
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].String() < invalid[j].String() })
	for i := 0; i < len(invalid); i++ {
		for _, child := range adjacent[invalid[i]] {
			if t.conflicts[child] {
				continue
			}
			t.conflicts[child] = true
			invalid = append(invalid, child)
			n := t.s.Nodes[child]
			n.Rendered, n.Discovered, n.Resources = false, false, nil
			n.SkipReason = ""
			n.Ambiguous = true
			n.Err = domain.Fail(domain.CodeGraphAncestorConflict, "graph", "an ancestor has conflicting desired specs", "resolve the conflicting ancestor before comparing its descendants", nil)
			t.fault(child, n.Expansion, n.Err)
		}
	}
}

// checkSharedDepth faults nodes deeper than MaxDepth on their longest path.
// A cached shared subtree may first be reached by a shorter path, so longest
// DAG paths are computed in topological order, without exponentially
// replaying shared branches. It consumes indegree.
func (t *walk) checkSharedDepth(adjacent map[domain.ApplicationID][]domain.ApplicationID, indegree map[domain.ApplicationID]int) {
	depth := map[domain.ApplicationID]int{}
	queue := []domain.ApplicationID{}
	for id, count := range indegree {
		if count == 0 {
			queue = append(queue, id)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].String() < queue[j].String() })
	for start := 0; start < len(queue); start++ {
		id := queue[start]
		if depth[id] > t.MaxDepth {
			t.fault(id, t.s.Nodes[id].Expansion, domain.LimitError(domain.CodeGraphLimitExceeded, "graph", "Application graph depth through a shared subtree", "render.maxDepth", int64(depth[id]), int64(t.MaxDepth), "levels", false, domain.MaxRenderDepth))
		}
		for _, child := range adjacent[id] {
			depth[child] = max(depth[child], depth[id]+1)
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

func validDNSName(value string, maxLength int, dots bool) bool {
	if len(value) == 0 || len(value) > maxLength || (!dots && strings.Contains(value, ".")) {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) > 63 || !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}
