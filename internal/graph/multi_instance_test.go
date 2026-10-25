package graph

import (
	"context"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestCrossInstanceCyclePreservesIndependentRoot(t *testing.T) {
	left, right, independent := app("root"), app("middle"), app("root")
	left.Spec["destination"] = map[string]any{"name": "right-management"}
	right.ID.Instance = "two"
	right.Spec["destination"] = map[string]any{"name": "left-management"}
	independent.ID.Instance = "two"
	tree := map[domain.ApplicationID][]domain.Application{left.ID: {right}, right.ID: {left}}
	routing := profile.Routing{Clusters: []profile.ClusterRoute{
		{ParentInstance: "one", Cluster: "right-management", Namespace: "argocd", Instance: "two"},
		{ParentInstance: "two", Cluster: "left-management", Namespace: "argocd", Instance: "one"},
	}}
	w := Walker{MaxDepth: 16, MaxApplications: 100, Route: routing.Route,
		Expand: func(_ context.Context, a domain.Application, _ bool) (Expansion, error) {
			return Expansion{Revision: "fixed-sha", Resources: children(tree[a.ID]...)}, nil
		},
	}
	snapshot := w.Walk(context.Background(), []domain.Application{left, independent})
	if snapshot.Complete || !hasFault(snapshot, "graph_cycle") || len(snapshot.Nodes) != 3 || !snapshot.Nodes[independent.ID].Rendered || !snapshot.Nodes[right.ID].Rendered {
		t.Fatal("cross-instance cycle hid an independent same-name root or bypassed cycle detection")
	}
}

func TestScopedRoutingKeepsSameNameApplicationsInDifferentNamespaces(t *testing.T) {
	root := app("root")
	root.Spec["destination"] = map[string]any{"name": "in-cluster"}
	first, second := app("api"), app("api")
	second.ID.Namespace = "team-apps"
	routing := profile.Routing{Clusters: []profile.ClusterRoute{
		{ParentInstance: "one", Cluster: "in-cluster", Namespace: "argocd", Instance: "two"},
		{ParentInstance: "one", Cluster: "in-cluster", Namespace: "team-apps", Instance: "two"},
	}}
	w := Walker{MaxDepth: 16, MaxApplications: 100, Route: routing.Route,
		Expand: func(_ context.Context, a domain.Application, isRoot bool) (Expansion, error) {
			if isRoot {
				return Expansion{Resources: children(first, second)}, nil
			}
			return Expansion{Resources: children()}, nil
		},
	}
	snapshot := w.Walk(context.Background(), []domain.Application{root})
	first.ID.Instance, second.ID.Instance = "two", "two"
	if !snapshot.Complete || len(snapshot.Nodes) != 3 || !snapshot.Nodes[first.ID].Rendered || !snapshot.Nodes[second.ID].Rendered {
		t.Fatal("Application namespace was dropped from the global visited identity")
	}
}
