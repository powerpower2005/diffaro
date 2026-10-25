package preview

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/security"
)

// An Application found on one side only, below a parent that failed on the
// other side, names that parent as the reason its absence is unproven. Without such an ancestor the reason stays generic.
func TestPresenceUnknownNamesTheUndiscoveredAncestor(t *testing.T) {
	id := func(name string) domain.ApplicationID {
		return domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: name}
	}
	node := func(name string, discovered bool, path ...string) *graph.Node {
		n := &graph.Node{Application: domain.Application{ID: id(name), Spec: map[string]any{"project": "demo"}}, Root: id("root"), Rendered: discovered, Discovered: discovered, Expansion: graph.Expansion{Repository: "https://github.com/example/charts.git", Revision: "fixed-sha"}}
		for _, p := range path {
			n.Path = append(n.Path, id(p))
		}
		return n
	}
	base := graph.Snapshot{Complete: true, Nodes: map[domain.ApplicationID]*graph.Node{
		id("root"): node("root", true, "root"), id("c1"): node("c1", true, "root", "c1"), id("c2"): node("c2", true, "root", "c1", "c2"),
	}}
	denied := domain.Fail(domain.CodeAccessDenied, "permission", "denied", "grant read", nil)
	head := graph.Snapshot{Faults: []graph.Fault{{ID: id("c1"), Err: denied}}, Nodes: map[domain.ApplicationID]*graph.Node{
		id("root"): node("root", true, "root"), id("c1"): node("c1", false, "root", "c1"),
	}}
	head.Nodes[id("c1")].Err = denied

	out := compareNode(base.Nodes[id("c2")], nil, nodePresence(base, head, nil, nil, id("c2")), security.Redactor{})
	if len(out.Issues) != 1 || out.Issues[0].Code != domain.CodeGraphPresenceUnknown || !strings.HasSuffix(out.Issues[0].Reason, "; its ancestor one/argocd/c1 was not discovered on that revision") || !strings.Contains(out.Issues[0].Action, "one/argocd/c1") {
		t.Fatalf("incomplete ancestor not named: %+v", out.Issues)
	}

	// Discovered ancestors give no name.
	head.Nodes[id("c1")].Discovered, head.Nodes[id("c1")].Err = true, nil
	out = compareNode(base.Nodes[id("c2")], nil, nodePresence(base, head, nil, nil, id("c2")), security.Redactor{})
	if len(out.Issues) != 1 || out.Issues[0].Reason != "incomplete opposite graph cannot prove this Application is absent" {
		t.Fatalf("an ancestor was named without evidence: %+v", out.Issues)
	}
}
