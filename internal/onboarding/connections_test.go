package onboarding

import (
	"os"
	"path/filepath"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestConnectionsProfileRequiresExplicitLocalChildDeclaration(t *testing.T) {
	instances := []profile.Instance{{ID: "dev", Endpoint: "https://dev.example.com", TokenEnv: "DEV_TOKEN"}, {ID: "prod", Endpoint: "https://prod.example.com", TokenEnv: "PROD_TOKEN"}}
	for _, local := range []bool{false, true} {
		config, err := ConnectionsProfile(profile.Connections{Instances: instances}, "https://github.com/example/charts/pull/12", "", "GIT_TOKEN", local, "team-apps", profile.DefaultRender())
		if err != nil || config.Discovery == nil || len(config.Instances) != 2 || len(config.Applications) != 0 || config.Routing.DefaultInstance != "" {
			t.Fatal("catalog generated implicit roots or a global manager")
		}
		for _, instance := range instances {
			for _, cluster := range []string{"in-cluster", "https://kubernetes.default.svc", "unknown"} {
				parent := domain.Application{ID: domain.ApplicationID{Instance: instance.ID}, Spec: map[string]any{"destination": map[string]any{"name": cluster}}}
				child := domain.Application{ID: domain.ApplicationID{Namespace: "team-apps"}}
				manager, _, err := config.Routing.Route(parent, child)
				if local && cluster != "unknown" {
					if err != nil || manager != instance.ID {
						t.Fatal("declared local route lost instance isolation")
					}
				} else if err == nil {
					t.Fatal("an undeclared path guessed its managing instance")
				}
			}
		}
	}
}

func TestConnectionsProfileValidatesEveryInstanceAndRoute(t *testing.T) {
	first := profile.Instance{ID: "dev", Endpoint: "https://dev.example.com", TokenEnv: "DEV_TOKEN"}
	for _, catalog := range []profile.Connections{
		{}, {Instances: []profile.Instance{first, first}},
		{Instances: []profile.Instance{first, {ID: "prod", Endpoint: "http://prod.example.com", TokenEnv: "PROD_TOKEN"}}},
		{Instances: []profile.Instance{first}, Routing: profile.Routing{DefaultInstance: "missing"}},
	} {
		if _, err := ConnectionsProfile(catalog, "https://github.com/example/charts/pull/12", "", "GIT_TOKEN", false, "argocd", profile.DefaultRender()); err == nil {
			t.Fatal("invalid multi-instance catalog accepted")
		}
	}
}

func TestConnectionCatalogGeneratesValidMultiInstanceProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.yaml")
	if err := os.WriteFile(path, []byte(`# Trusted server catalog, reusable across PR repositories. Store token values
# in environment variables; optional repoServer setup belongs to each instance.
instances:
  - id: dev
    endpoint: https://argocd-dev.example.com
    tokenEnv: DIFFARO_ARGOCD_DEV_TOKEN
  - id: prod
    endpoint: https://argocd-prod.example.com
    tokenEnv: DIFFARO_ARGOCD_PROD_TOKEN
`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := profile.LoadConnections(path)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ConnectionsProfile(catalog, "https://github.com/example/charts/pull/12", "", "GIT_TOKEN", true, "argocd", profile.DefaultRender())
	if err != nil || len(config.Instances) != 2 || len(config.Routing.Clusters) != 4 {
		t.Fatal("catalog did not generate the expected profile")
	}
}
