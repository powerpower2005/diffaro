package preview

import (
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/profile"
)

func TestIncompleteGraphScopeIncludesEveryPotentialCrossInstanceRoute(t *testing.T) {
	instances := []profile.Instance{{ID: "failed"}, {ID: "healthy"}, {ID: "bridge"}}
	for _, tc := range []struct {
		name        string
		routing     profile.Routing
		wantHealthy bool
	}{
		{"local only", profile.Routing{Clusters: []profile.ClusterRoute{{ParentInstance: "failed", Instance: "failed"}, {ParentInstance: "healthy", Instance: "healthy"}}}, true},
		{"scoped cross instance", profile.Routing{Clusters: []profile.ClusterRoute{{ParentInstance: "failed", Instance: "healthy"}}}, false},
		{"transitive cross instance", profile.Routing{Clusters: []profile.ClusterRoute{{ParentInstance: "failed", Instance: "bridge"}, {ParentInstance: "bridge", Instance: "healthy"}}}, false},
		{"unscoped cluster", profile.Routing{Clusters: []profile.ClusterRoute{{Instance: "healthy"}}}, false},
		{"repository", profile.Routing{Repositories: []profile.RepositoryRoute{{Instance: "healthy"}}}, false},
		{"parent metadata", profile.Routing{Parents: []profile.ParentRoute{{Parent: domain.ApplicationID{Instance: "failed", Name: "unseen"}, Instance: "healthy"}}}, false},
		{"default", profile.Routing{DefaultInstance: "healthy"}, false},
	} {
		for _, skipped := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: " failure", true: " skip"}[skipped], func(t *testing.T) {
				id := domain.ApplicationID{Instance: "failed", Name: "root"}
				snapshot := graph.Snapshot{Nodes: map[domain.ApplicationID]*graph.Node{id: {}}}
				if !skipped {
					snapshot.Faults = []graph.Fault{{ID: id}}
				}
				complete := completeInstances(snapshot, tc.routing, instances)
				if complete["failed"] || complete["healthy"] != tc.wantHealthy {
					t.Fatalf("unsafe completeness: %+v", complete)
				}
			})
		}
	}
}

func TestIncompleteGraphWithoutBoundedScopedEvidenceCannotProveAbsence(t *testing.T) {
	for _, snapshot := range []graph.Snapshot{
		{}, {Faults: []graph.Fault{{}}}, {Faults: make([]graph.Fault, 128)},
		{Nodes: map[domain.ApplicationID]*graph.Node{{Name: "unknown"}: {}}},
	} {
		if completeInstances(snapshot, profile.Routing{}, []profile.Instance{{ID: "healthy"}})["healthy"] {
			t.Fatal("unscoped or truncated discovery proved absence")
		}
	}
}
