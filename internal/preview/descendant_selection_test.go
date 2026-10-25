package preview

import (
	"context"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/manifest"
	"diffaro/internal/profile"
)

const otherCommit = "cccccccccccccccccccccccccccccccccccccccc"

func otherApp(name string) domain.Application {
	app := graphApp(name)
	app.Spec["source"] = map[string]any{"repoURL": "https://github.com/example/other.git", "path": "charts/" + name, "targetRevision": otherCommit, "helm": map[string]any{}}
	return app
}

// A rendered App-of-Apps parent no longer forces its whole subtree to render:
// children with the same spec on both revisions and unchanged inputs are
// skipped on both sides, while changed specs, changed PR inputs, unknown
// status and children of an unstable parent still render.
func TestDescendantSelectionSkipsOnlyProvenIdenticalChildren(t *testing.T) {
	rendered := map[string]int{}
	var mu sync.Mutex
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		mu.Lock()
		rendered[app.ID.Name]++
		mu.Unlock()
		switch app.ID.Name {
		case "parent":
			tuned, mid := otherApp("tuned"), otherApp("mid")
			if rev == graphHead {
				tuned.Source()["helm"] = map[string]any{"releaseName": "new"}
				mid.Source()["helm"] = map[string]any{"releaseName": "new"}
			}
			return []string{appManifest(otherApp("quiet")), appManifest(tuned), appManifest(graphApp("local")), appManifest(graphApp("idle")), appManifest(otherApp("unlisted")), appManifest(mid)}, nil
		case "mid":
			return []string{appManifest(otherApp("leaf"))}, nil
		case "local":
			return []string{configMap(rev)}, nil
		default:
			return []string{configMap("same")}, nil
		}
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"charts/local/values.yaml"}, Read: func(_ context.Context, file string) ([]byte, bool, error) {
		return []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n"), strings.HasSuffix(file, "/Chart.yaml"), nil
	}}
	no, yes := false, true
	inv := &fakeInventory{}
	for _, item := range []struct {
		app     domain.Application
		manages *bool
	}{{graphApp("parent"), &yes}, {otherApp("quiet"), &no}, {otherApp("tuned"), &no}, {graphApp("local"), &no}, {graphApp("idle"), &no}, {otherApp("mid"), &yes}, {otherApp("leaf"), &no}} {
		inv.apps = append(inv.apps, domain.OwnedApplication{Application: item.app, ManagesApplications: item.manages})
	}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	for _, name := range []string{"quiet", "idle"} {
		if rendered[name] != 0 {
			t.Fatalf("%s rendered although proven identical: %v", name, rendered)
		}
	}
	for _, name := range []string{"parent", "tuned", "local", "unlisted", "mid", "leaf"} {
		if rendered[name] == 0 {
			t.Fatalf("%s skipped without proof: %v", name, rendered)
		}
	}
	if report.Status != "success" || !report.Graph.BaseComplete || !report.Graph.HeadComplete || !report.ChangesFound {
		t.Fatalf("unexpected report: status=%s graph=%+v", report.Status, report.Graph)
	}
	for _, app := range report.Applications {
		if app.ID.Name == "quiet" || app.ID.Name == "idle" {
			t.Fatalf("skipped descendant reported as an Application result: %+v", app)
		}
	}
	for _, edge := range report.Graph.Changes {
		if edge.State != "unchanged" {
			t.Fatalf("skipping changed graph edges: %+v", report.Graph.Changes)
		}
	}
	decisions := map[string]domain.DiscoveryDecision{}
	for _, decision := range report.Discovery[0].Decisions {
		decisions[decision.ID.Name] = decision
	}
	if quiet := decisions["quiet"]; quiet.Selection != selectionUnaffected || !strings.HasPrefix(quiet.SelectionReason, "descendant of one/argocd/parent") {
		t.Fatalf("skipped descendant decision: %+v", quiet)
	}
	if idle := decisions["idle"]; idle.Selection != selectionUnaffected || strings.HasPrefix(idle.SelectionReason, "descendant") {
		t.Fatalf("candidate decision overwritten: %+v", idle)
	}
	if report.Discovery[0].Unaffected != 1 || report.Discovery[0].UnaffectedDescendants != 1 {
		t.Fatalf("counts: %+v", report.Discovery[0])
	}
}

// Without changed-path selection every descendant still renders.
func TestDescendantSelectionRequiresChangedPaths(t *testing.T) {
	rendered := map[string]int{}
	var mu sync.Mutex
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		mu.Lock()
		rendered[app.ID.Name]++
		mu.Unlock()
		if app.ID.Name == "parent" {
			return []string{appManifest(otherApp("quiet"))}, nil
		}
		return []string{configMap("same")}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	no, yes := false, true
	inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("parent"), ManagesApplications: &yes}, {Application: otherApp("quiet"), ManagesApplications: &no}}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if rendered["quiet"] == 0 || report.Discovery[0].UnaffectedDescendants != 0 {
		t.Fatalf("descendant skipped without opt-in: %v %+v", rendered, report.Discovery[0])
	}
}

// A candidate probe walks each candidate as a root, where a child may be
// skipped. When the final walk reaches that candidate as a changed child, its
// children render and must not keep the probe's unaffected decision.
func TestDescendantSelectionIgnoresProbeOnlySkips(t *testing.T) {
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		switch app.ID.Name {
		case "top":
			mid := graphApp("mid")
			if rev == graphHead {
				mid.Source()["helm"] = map[string]any{"releaseName": "new"}
			}
			return []string{appManifest(mid)}, nil
		case "mid":
			return []string{appManifest(otherApp("leaf"))}, nil
		default:
			return []string{configMap("same")}, nil
		}
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"README.md"}, Read: func(_ context.Context, file string) ([]byte, bool, error) {
		return []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n"), strings.HasSuffix(file, "/Chart.yaml"), nil
	}}
	no, yes := false, true
	inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("top"), ManagesApplications: &yes}, {Application: graphApp("mid"), ManagesApplications: &yes}, {Application: otherApp("leaf"), ManagesApplications: &no}}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if leaf := findApp(t, report, "leaf"); leaf.Execution != "complete" {
		t.Fatalf("leaf was not compared: %+v", leaf)
	}
	for _, decision := range report.Discovery[0].Decisions {
		if decision.ID.Name == "leaf" && decision.Selection == selectionUnaffected {
			t.Fatalf("rendered descendant reported unaffected: %+v", decision)
		}
	}
	if report.Discovery[0].UnaffectedDescendants != 0 {
		t.Fatalf("counts: %+v", report.Discovery[0])
	}
}

// One walk may fail to render the opposite parent while the other succeeds.
// The decision for a parent/child pair is made once, so both walks agree.
func TestDescendantSkipDecidesOncePerPair(t *testing.T) {
	selector := newChangedPathSelector(ChangedPaths{Listed: true, Read: func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }}, func(string) bool { return false })
	no := false
	d := newDescendantSelector(selector, graphPair(), map[domain.ApplicationID]bool{otherApp("leaf").ID: no}, nil)
	parent, child := graphApp("parent"), otherApp("leaf")
	calls := 0
	opposite := func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		calls++
		if calls == 1 {
			return graph.Expansion{}, context.DeadlineExceeded
		}
		resources := map[string]manifest.Resource{}
		id := domain.ResourceID{Group: "argoproj.io", Kind: "Application", Namespace: child.ID.Namespace, Name: child.ID.Name}
		resources[id.String()] = manifest.Resource{ID: id, Object: map[string]any{"spec": child.Spec}}
		return graph.Expansion{Resources: resources}, nil
	}
	base := d.walkSkip(graphBase, opposite)(context.Background(), parent, true, child)
	head := d.walkSkip(graphHead, opposite)(context.Background(), parent, true, child)
	if base != head {
		t.Fatalf("base and head walks decided differently: base=%v head=%v", base, head)
	}
}

// If the walks still decide differently (for example after a cancelled
// wait), a child or descendant skipped on one side is unknown there, never
// removed, added or pruned.
func TestOneSidedDescendantSkipIsNotAnAbsence(t *testing.T) {
	parent, child, grandchild := graphApp("parent"), otherApp("leaf"), otherApp("deep")
	cm := domain.ResourceID{Cluster: "one:management", Kind: "ConfigMap", Namespace: "argocd", Name: "settings"}
	rendered := func(app domain.Application, path ...domain.ApplicationID) *graph.Node {
		return &graph.Node{Application: app, Root: parent.ID, Path: path, Rendered: true, Discovered: true, Expansion: graph.Expansion{Resources: map[string]manifest.Resource{cm.String(): {ID: cm, Object: map[string]any{"data": map[string]any{"setting": "same"}}}}}}
	}
	skipped := graph.Snapshot{Nodes: map[domain.ApplicationID]*graph.Node{parent.ID: rendered(parent, parent.ID)}, Skipped: map[domain.ApplicationID]domain.ApplicationID{child.ID: parent.ID}, Complete: true}
	visited := graph.Snapshot{Nodes: map[domain.ApplicationID]*graph.Node{child.ID: rendered(child, parent.ID, child.ID), grandchild.ID: rendered(grandchild, parent.ID, child.ID, grandchild.ID)}, Complete: true}
	runner := graphRunner(graphBackend{})
	for _, id := range []domain.ApplicationID{child.ID, grandchild.ID} {
		node := visited.Nodes[id]
		results := map[string]domain.ApplicationResult{
			"base skipped": compareNode(nil, node, presence{base: sidePresence{complete: true, skipped: skippedOn(skipped, node)}, head: sidePresence{complete: true}}, runner.Redactor),
			"head skipped": compareNode(node, nil, presence{base: sidePresence{complete: true}, head: sidePresence{complete: true, skipped: skippedOn(skipped, node)}}, runner.Redactor),
		}
		for name, app := range results {
			if app.State != "unknown" || app.Execution != "skipped" || app.SkipReason == "" || app.Pruning != nil || len(app.Resources) != 0 {
				t.Fatalf("%s %s: one-sided skip treated as a proven absence: %+v", id.Name, name, app)
			}
		}
	}
	if skippedOn(skipped, rendered(otherApp("elsewhere"), parent.ID, otherApp("elsewhere").ID)) {
		t.Fatal("unrelated missing node treated as skipped")
	}
}
