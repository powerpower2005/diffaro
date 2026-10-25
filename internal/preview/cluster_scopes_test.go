package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
)

func scopedApp(name, server string, scopes map[string]bool) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}, ObservedScopes: scopes,
		Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/other.git", "path": name, "helm": map[string]any{}}, "destination": map[string]any{"server": server, "namespace": "demo"}}}
}

func TestClusterScopesAggregateAgreeingObservationsPerCluster(t *testing.T) {
	scopes := aggregateScopes("east", []domain.Application{
		scopedApp("a", "https://one.example", map[string]bool{"networking.istio.io/Sidecar": false, "example.io/Global": true}),
		scopedApp("b", "https://one.example", map[string]bool{"networking.istio.io/Sidecar": false, "example.io/Mixed": true}),
		scopedApp("c", "https://one.example", map[string]bool{"example.io/Mixed": false}),
		scopedApp("d", "https://two.example", map[string]bool{"example.io/Other": false}),
	})
	one := scopes["east:https://one.example"]
	if clusterScoped, ok := one["networking.istio.io/Sidecar"]; !ok || clusterScoped {
		t.Fatal("agreeing namespaced observations must be kept")
	}
	if _, ok := one["example.io/Mixed"]; ok {
		t.Fatal("conflicting observations must stay unknown")
	}
	if _, ok := one["example.io/Other"]; ok || len(scopes["east:https://two.example"]) != 1 {
		t.Fatal("clusters must not share observations")
	}
	own := scopedApp("e", "https://one.example", map[string]bool{"example.io/Global": false})
	merged := scopes.scopesFor(own, "east:https://one.example")
	if merged["example.io/Global"] || !(merged["networking.istio.io/Sidecar"] == false) || len(merged) != 2 {
		t.Fatalf("the Application's own observation must win and the cluster aggregate fill the rest: %v", merged)
	}
	if got := scopes.scopesFor(own, "east:https://unknown.example"); len(got) != 1 {
		t.Fatal("an unknown cluster contributes nothing")
	}
}

func TestRepositoryDiscoveryAggregatesUnselectedApplicationScopes(t *testing.T) {
	inventory := &fakeInventory{apps: []domain.OwnedApplication{
		{Application: scopedApp("platform", "https://one.example", map[string]bool{"networking.istio.io/Sidecar": false})},
	}}
	snapshot, err := discoverRepository(context.Background(), discoveryScope{Instance: "east"}, "https://github.com/example/charts.git", inventory, 10)
	if err != nil || len(snapshot.Applications) != 0 {
		t.Fatalf("unexpected selection: %v %v", err, snapshot.Applications)
	}
	if _, ok := snapshot.Scopes["east:https://one.example"]["networking.istio.io/Sidecar"]; !ok {
		t.Fatal("observed scopes of unselected Applications must be aggregated")
	}
}
