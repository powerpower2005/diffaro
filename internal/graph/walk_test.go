package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
)

func app(name string) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: name}, Spec: map[string]any{"project": "demo"}}
}
func children(apps ...domain.Application) map[string]manifest.Resource {
	resources := map[string]manifest.Resource{}
	for _, a := range apps {
		id := domain.ResourceID{Group: "argoproj.io", Kind: "Application", Namespace: a.ID.Namespace, Name: a.ID.Name}
		resources[id.String()] = manifest.Resource{ID: id, Object: map[string]any{"spec": a.Spec}}
	}
	return resources
}
func fixture(tree map[string][]domain.Application, calls map[string]int) Walker {
	return Walker{MaxDepth: 16, MaxApplications: 100,
		Route: func(parent, child domain.Application) (string, string, error) {
			return parent.ID.Instance, "fixture", nil
		},
		Expand: func(_ context.Context, a domain.Application, _ bool) (Expansion, error) {
			calls[a.ID.Name]++
			return Expansion{Repository: "https://example.com/charts.git", Revision: "fixed-sha", Resources: children(tree[a.ID.Name]...)}, nil
		},
	}
}
func hasFault(s Snapshot, code string) bool {
	for _, fault := range s.Faults {
		var f *domain.Failure
		if errors.As(fault.Err, &f) && f.Issue.Code == code {
			return true
		}
	}
	return false
}

func TestSkippedRenderKeepsEdgesWithoutInventingDescendants(t *testing.T) {
	calls := map[string]int{}
	w := fixture(map[string][]domain.Application{"root": {app("skipped"), app("independent")}}, calls)
	expand := w.Expand
	w.Expand = func(ctx context.Context, a domain.Application, root bool) (Expansion, error) {
		if a.ID.Name == "skipped" {
			return Expansion{Resources: children(app("untrusted-descendant"))}, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
		}
		return expand(ctx, a, root)
	}
	snapshot := w.Walk(context.Background(), []domain.Application{app("root")})
	skipped := snapshot.Nodes[app("skipped").ID]
	if snapshot.Complete || len(snapshot.Faults) != 0 || len(snapshot.Nodes) != 3 || len(snapshot.Edges) != 2 || skipped.Rendered || skipped.Discovered || skipped.Err != nil || skipped.SkipReason == "" || len(skipped.Resources) != 0 || !snapshot.Nodes[app("independent").ID].Rendered {
		t.Fatal("skip invented targets, failed the run, or lost the independent branch")
	}
}

func TestSkippedSharedNodeStillRejectsConflictingSpecs(t *testing.T) {
	changed := app("skipped")
	changed.Spec["project"] = "other"
	w := fixture(map[string][]domain.Application{"root": {app("left"), app("right")}, "left": {app("skipped")}, "right": {changed}}, map[string]int{})
	expand := w.Expand
	w.Expand = func(ctx context.Context, a domain.Application, root bool) (Expansion, error) {
		if a.ID.Name == "skipped" {
			return Expansion{}, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
		}
		return expand(ctx, a, root)
	}
	snapshot := w.Walk(context.Background(), []domain.Application{app("root")})
	if !hasFault(snapshot, "graph_spec_conflict") || snapshot.Nodes[app("skipped").ID].SkipReason != "" {
		t.Fatal("skipped shared node hid a conflicting desired spec")
	}
}

func TestDeepGraphAndSharedNode(t *testing.T) {
	calls := map[string]int{}
	w := fixture(map[string][]domain.Application{"root": {app("a"), app("b")}, "a": {app("leaf")}, "b": {app("leaf")}, "leaf": {app("deep")}}, calls)
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if !s.Complete || len(s.Nodes) != 5 || len(s.Edges) != 5 || calls["leaf"] != 1 || len(s.Nodes[app("deep").ID].Path) != 4 {
		t.Fatalf("recursive discovery or DAG reuse failed: %+v", s)
	}
	if s.Edges[0].Repository == "" || s.Edges[0].Revision == "" || s.Edges[0].Method != "rendered-application" {
		t.Fatal("edge provenance missing")
	}
}

func TestGraphFailuresPreserveIndependentNodes(t *testing.T) {
	for _, test := range []struct {
		name, code          string
		depth, count, bytes int
		cycle               bool
	}{
		{"depth", "graph_limit_exceeded", 1, 100, 0, false},
		{"count", "graph_limit_exceeded", 16, 2, 0, false},
		{"bytes", "graph_limit_exceeded", 16, 100, 1, false},
		{"cycle", "graph_cycle", 16, 100, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := map[string][]domain.Application{"root": {app("a")}, "a": {app("deep")}}
			if test.cycle {
				tree["a"] = []domain.Application{app("root")}
			}
			w := fixture(tree, map[string]int{})
			w.MaxDepth, w.MaxApplications, w.MaxBytes = test.depth, test.count, test.bytes
			s := w.Walk(context.Background(), []domain.Application{app("root")})
			if s.Complete || !hasFault(s, test.code) {
				t.Fatal("failed graph advertised complete coverage")
			}
			if test.bytes == 0 && !s.Nodes[app("root").ID].Rendered {
				t.Fatal("successful root was discarded")
			}
		})
	}
}

// A graph limit failure records the walk line that hit it, not the limit
// constructor in domain.
func TestGraphLimitSiteIsTheWalk(t *testing.T) {
	w := fixture(map[string][]domain.Application{"root": {app("a")}, "a": {app("deep")}}, map[string]int{})
	w.MaxDepth = 1
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	var sites []string
	for _, fault := range s.Faults {
		var f *domain.Failure
		if errors.As(fault.Err, &f) && f.Issue.Code == domain.CodeGraphLimitExceeded && f.Issue.Diagnostic != nil {
			sites = append(sites, f.Issue.Diagnostic.Site)
		}
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "graph/walk.go:") {
		t.Fatalf("limit sites %q, want graph/walk.go", sites)
	}
}

// A cycle names its path from the repeated Application's first occurrence,
// so the parent that defines it again is visible without JSON.
func TestCycleNamesItsPath(t *testing.T) {
	w := fixture(map[string][]domain.Application{"top": {app("root")}, "root": {app("c1")}, "c1": {app("c2")}, "c2": {app("root")}}, map[string]int{})
	s := w.Walk(context.Background(), []domain.Application{app("top")})
	var reasons []string
	for _, fault := range s.Faults {
		var f *domain.Failure
		if errors.As(fault.Err, &f) && f.Issue.Code == domain.CodeGraphCycle {
			reasons = append(reasons, f.Issue.Reason)
		}
	}
	const want = "Application identity repeats on the active graph path: one/argocd/root -> one/argocd/c1 -> one/argocd/c2 -> one/argocd/root"
	if len(reasons) != 1 || reasons[0] != want {
		t.Fatalf("cycle reasons %q, want %q", reasons, want)
	}
}

func TestConflictingSpecIsNotReused(t *testing.T) {
	other := app("leaf")
	other.Spec = map[string]any{"project": "different"}
	w := fixture(map[string][]domain.Application{"root": {app("a"), app("b")}, "a": {app("leaf")}, "b": {other}, "leaf": {app("descendant")}}, map[string]int{})
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if !hasFault(s, "graph_spec_conflict") || s.Nodes[other.ID].Rendered || s.Nodes[other.ID].Resources != nil {
		t.Fatal("conflicting desired spec was silently reused")
	}
	if s.Nodes[app("descendant").ID].Rendered || !hasFault(s, "graph_ancestor_conflict") {
		t.Fatal("descendants of an ambiguous spec were reported as reliable")
	}
	for _, change := range CompareEdges(s, s) {
		if change.Parent == other.ID && change.State != "unknown" {
			t.Fatal("ambiguous parent's cached edge was reported as reliable")
		}
	}
}

func TestSharedSubtreeCannotBypassDepthLimit(t *testing.T) {
	w := fixture(map[string][]domain.Application{"root": {app("a"), app("z")}, "a": {app("leaf")}, "z": {app("a")}}, map[string]int{})
	w.MaxDepth = 2
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if s.Complete || !hasFault(s, "graph_limit_exceeded") {
		t.Fatal("cached subtree hid an over-depth path")
	}
}

func TestInvalidChildIdentityMakesNoLookup(t *testing.T) {
	calls := map[string]int{}
	w := fixture(map[string][]domain.Application{"root": {app("../projects")}}, calls)
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if !hasFault(s, "child_identity_invalid") || len(calls) != 1 || s.Nodes[app("root").ID].Discovered {
		t.Fatal("unsafe child identity reached a provider")
	}
}

func TestEdgeRemovalIsIndependentOfUnrelatedDiscoveryFailure(t *testing.T) {
	left := fixture(map[string][]domain.Application{"root": {app("child")}}, map[string]int{}).Walk(context.Background(), []domain.Application{app("root")})
	right := fixture(nil, map[string]int{}).Walk(context.Background(), []domain.Application{app("root")})
	right.Complete = false // unrelated branch failed, but root's direct children are known.
	changes := CompareEdges(left, right)
	if len(changes) != 1 || changes[0].State != "removed" {
		t.Fatal("known edge removal hidden by independent failure")
	}
	right.Nodes[app("root").ID].Discovered = false
	if CompareEdges(left, right)[0].State != "unknown" {
		t.Fatal("failed direct discovery fabricated edge removal")
	}
}

func TestSameNamesAcrossInstancesAreDistinct(t *testing.T) {
	a, b := app("root"), app("root")
	b.ID.Instance = "two"
	w := fixture(map[string][]domain.Application{"root": {app("leaf")}}, map[string]int{})
	s := w.Walk(context.Background(), []domain.Application{a, b})
	if !s.Complete || len(s.Nodes) != 4 {
		t.Fatal("global identity collapsed instances")
	}
}

func TestUnsupportedGeneratorsAndRouting(t *testing.T) {
	for _, routeFailure := range []bool{false, true} {
		w := fixture(map[string][]domain.Application{"root": {app("child")}}, map[string]int{})
		code := "routing_missing"
		if routeFailure {
			w.Route = func(domain.Application, domain.Application) (string, string, error) {
				return "", "", domain.Fail(code, "routing", "missing", "configure route", nil)
			}
		} else {
			code = "unsupported_applicationset"
			w.Expand = func(context.Context, domain.Application, bool) (Expansion, error) {
				id := domain.ResourceID{Group: "argoproj.io", Kind: "ApplicationSet", Name: "set"}
				return Expansion{Resources: map[string]manifest.Resource{id.String(): {ID: id}}}, nil
			}
		}
		s := w.Walk(context.Background(), []domain.Application{app("root")})
		if s.Complete || !hasFault(s, code) || !s.Nodes[app("root").ID].Rendered {
			t.Fatal("incomplete discovery lost successful target or missing error")
		}
	}
}

func TestCancelledGraphMakesNoProviderCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]int{}
	s := fixture(nil, calls).Walk(ctx, []domain.Application{app("root")})
	if s.Complete || len(calls) != 0 || len(s.Faults) != 1 || !errors.Is(s.Faults[0].Err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
}

func TestPrefetchReceivesUnvisitedChildrenWithinDepth(t *testing.T) {
	tree := map[string][]domain.Application{"root": {app("b"), app("a"), {ID: domain.ApplicationID{Namespace: "argocd", Name: "Invalid_Name"}, Spec: map[string]any{}}}, "a": {app("root"), app("leaf")}, "b": {app("a")}}
	sequential := fixture(tree, map[string]int{}).Walk(context.Background(), []domain.Application{app("root")})
	w := fixture(tree, map[string]int{})
	w.MaxDepth = 1
	var got [][]string
	w.Prefetch = func(_ context.Context, children []domain.Application) {
		var names []string
		for _, child := range children {
			names = append(names, child.ID.Name)
		}
		got = append(got, names)
	}
	w.Walk(context.Background(), []domain.Application{app("root")})
	// Root's valid children in visit order; a's children are beyond depth 1.
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != "a" || got[0][1] != "b" {
		t.Fatalf("prefetch must see the routable children the walk will visit next: %v", got)
	}
	w = fixture(tree, map[string]int{})
	w.Prefetch = func(context.Context, []domain.Application) {}
	if prefetched := w.Walk(context.Background(), []domain.Application{app("root")}); len(prefetched.Nodes) != len(sequential.Nodes) || len(prefetched.Edges) != len(sequential.Edges) || len(prefetched.Faults) != len(sequential.Faults) {
		t.Fatal("a prefetch hook must not change the walk")
	}
}

func TestSkippedChildKeepsEdgeWithoutVisitOrPrefetch(t *testing.T) {
	calls := map[string]int{}
	w := fixture(map[string][]domain.Application{"root": {app("quiet"), app("busy")}, "quiet": {app("deep")}}, calls)
	prefetched := []string{}
	w.Prefetch = func(_ context.Context, ahead []domain.Application) {
		for _, a := range ahead {
			prefetched = append(prefetched, a.ID.Name)
		}
	}
	w.Skip = func(_ context.Context, parent domain.Application, root bool, child domain.Application) bool {
		return parent.ID.Name == "root" && root && child.ID.Name == "quiet"
	}
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if !s.Complete || s.Nodes[app("quiet").ID] != nil || s.Nodes[app("deep").ID] != nil || calls["quiet"] != 0 || calls["busy"] != 1 {
		t.Fatalf("skipped child visited: nodes=%v calls=%v", s.Nodes, calls)
	}
	if len(s.Edges) != 2 || len(prefetched) != 1 || prefetched[0] != "busy" {
		t.Fatalf("edges=%v prefetched=%v", s.Edges, prefetched)
	}
}

// Snapshot.Skipped names only children that Skip omitted and no other path
// visited, so comparison can tell a skipped child from a proven absence.
func TestSkippedChildIsRecordedUnlessVisitedElsewhere(t *testing.T) {
	calls := map[string]int{}
	w := fixture(map[string][]domain.Application{"root": {app("quiet"), app("shared"), app("busy")}, "busy": {app("shared")}}, calls)
	w.Skip = func(_ context.Context, parent domain.Application, _ bool, child domain.Application) bool {
		return parent.ID.Name == "root"
	}
	s := w.Walk(context.Background(), []domain.Application{app("root"), app("busy")})
	if len(s.Skipped) != 1 || s.Skipped[app("quiet").ID] != app("root").ID {
		t.Fatalf("skipped children: %v", s.Skipped)
	}
	if s.Nodes[app("shared").ID] == nil {
		t.Fatal("child visited through another path is missing")
	}
}

// A child whose rendered desired spec holds a placeholder for fields that
// differ between two renders is rendered, not skipped as unchanged; it and
// its descendants name the placeholder, and a render failure becomes a skip
// while other failures are kept.
func TestPlaceholderSpecsAreNamedRenderedAndNotFailed(t *testing.T) {
	placeholder := func(name string) domain.Application {
		a := app(name)
		a.Spec["token"] = domain.RenderUnstable
		return a
	}
	tree := map[string][]domain.Application{"root": {placeholder("worker"), placeholder("failing"), placeholder("denied"), app("plain")}, "worker": {app("leaf")}}
	w := fixture(tree, map[string]int{})
	expand := w.Expand
	w.Expand = func(ctx context.Context, a domain.Application, root bool) (Expansion, error) {
		switch a.ID.Name {
		case "failing":
			return Expansion{}, domain.Fail("local_render_failed", "render", "template app/templates/config.yaml:3 failed", "fix the template", nil)
		case "denied":
			return Expansion{}, domain.Fail("access_denied", "authorization", "repository read denied", "grant read access", nil)
		}
		return expand(ctx, a, root)
	}
	w.Skip = func(context.Context, domain.Application, bool, domain.Application) bool { return true }
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	id := func(name string) domain.ApplicationID { return app(name).ID }
	worker, leaf, failing, denied := s.Nodes[id("worker")], s.Nodes[id("leaf")], s.Nodes[id("failing")], s.Nodes[id("denied")]
	if worker == nil || leaf == nil || failing == nil || denied == nil {
		t.Fatalf("a placeholder spec, or a descendant of one, was skipped as unchanged: %v", s.Skipped)
	}
	if _, skipped := s.Skipped[id("plain")]; !skipped || s.Nodes[id("plain")] != nil {
		t.Fatal("an ordinary unchanged child was rendered")
	}
	if p := worker.Placeholder; p == nil || p.Application != id("worker") || len(p.Paths) != 1 || p.Paths[0] != "/spec/token" {
		t.Fatalf("worker placeholder: %+v", worker.Placeholder)
	}
	if leaf.Placeholder != worker.Placeholder || s.Nodes[id("root")].Placeholder != nil {
		t.Fatal("a descendant did not inherit the placeholder, or the root gained one")
	}
	if failing.Err != nil || !strings.Contains(failing.SkipReason, "placeholder") || !strings.Contains(failing.SkipReason, "template app/templates/config.yaml:3 failed") {
		t.Fatalf("render failure from a placeholder: err %v, skip %q", failing.Err, failing.SkipReason)
	}
	if !hasFault(s, "access_denied") || denied.SkipReason != "" {
		t.Fatal("denied access was turned into a skip")
	}
}

// Skip, Prefetch and edge linking share one routing decision per declared
// child instead of resolving each resource again.
func TestChildRoutingIsResolvedOncePerResource(t *testing.T) {
	w := fixture(map[string][]domain.Application{"root": {app("a"), app("b")}, "a": {app("leaf")}}, map[string]int{})
	routed := map[string]int{}
	route := w.Route
	w.Route = func(parent, child domain.Application) (string, string, error) {
		routed[parent.ID.Name+"->"+child.ID.Name]++
		return route(parent, child)
	}
	w.Prefetch = func(context.Context, []domain.Application) {}
	w.Skip = func(context.Context, domain.Application, bool, domain.Application) bool { return false }
	s := w.Walk(context.Background(), []domain.Application{app("root")})
	if !s.Complete || len(s.Nodes) != 4 || len(s.Edges) != 3 {
		t.Fatalf("walk changed: nodes=%d edges=%d complete=%v", len(s.Nodes), len(s.Edges), s.Complete)
	}
	if len(routed) != 3 || routed["root->a"] != 1 || routed["root->b"] != 1 || routed["a->leaf"] != 1 {
		t.Fatalf("each declared child must be routed once: %v", routed)
	}
}
