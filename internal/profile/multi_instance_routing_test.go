package profile

import (
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestInstanceScopedClusterRouting(t *testing.T) {
	routes := []ClusterRoute{
		{ParentInstance: "dev", Cluster: "in-cluster", Namespace: "argocd", Instance: "dev"},
		{ParentInstance: "prod", Cluster: "in-cluster", Namespace: "argocd", Instance: "prod"},
		{ParentInstance: "dev", Cluster: "remote-management", Namespace: "argocd", Instance: "prod"},
		{ParentInstance: "dev", Cluster: "in-cluster", Namespace: "other-argocd", Instance: "prod"},
	}
	for _, tc := range []struct {
		name, parentInstance, cluster, namespace, want, code string
	}{
		{"dev local alias", "dev", "in-cluster", "argocd", "dev", ""},
		{"prod same alias", "prod", "in-cluster", "argocd", "prod", ""},
		{"cross-instance edge", "dev", "remote-management", "argocd", "prod", ""},
		{"Application namespace", "dev", "in-cluster", "other-argocd", "prod", ""},
		{"unknown parent", "third", "in-cluster", "argocd", "", "routing_missing"},
		{"unknown cluster", "dev", "unknown", "argocd", "", "routing_missing"},
		{"unknown namespace", "dev", "in-cluster", "unknown", "", "routing_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := domain.Application{ID: domain.ApplicationID{Instance: tc.parentInstance, Namespace: "argocd", Name: "root"}, Spec: map[string]any{"destination": map[string]any{"name": tc.cluster}}}
			child := domain.Application{ID: domain.ApplicationID{Namespace: tc.namespace, Name: "api"}, Spec: map[string]any{"destination": map[string]any{"name": "in-cluster"}}}
			got, rule, err := (Routing{Clusters: routes}).Route(parent, child)
			if tc.code == "" {
				if err != nil || got != tc.want || rule != "cluster" {
					t.Fatalf("route=%s/%s err=%v", got, rule, err)
				}
			} else {
				var failure *domain.Failure
				if got != "" || rule != "" || !errors.As(err, &failure) || failure.Issue.Code != tc.code || !strings.Contains(failure.Issue.Action, "parentInstance=") {
					t.Fatalf("missing route guessed an instance or omitted the selector: %v", err)
				}
			}
		})
	}
}

func TestScopedRoutesKeepExistingPrecedenceAndRejectConflicts(t *testing.T) {
	parent := domain.Application{ID: domain.ApplicationID{Instance: "dev", Namespace: "argocd", Name: "root"}, Spec: map[string]any{"destination": map[string]any{"server": "https://management.example.com", "name": "in-cluster"}}}
	child := domain.Application{ID: domain.ApplicationID{Namespace: "argocd", Name: "api"}, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git"}}}
	matching := ClusterRoute{ParentInstance: "dev", Cluster: "https://management.example.com", Namespace: "argocd", Instance: "dev"}
	for _, tc := range []struct {
		name             string
		routes           Routing
		want, rule, code string
	}{
		{"legacy unscoped", Routing{Clusters: []ClusterRoute{{Cluster: matching.Cluster, Namespace: "argocd", Instance: "prod"}}}, "prod", "cluster", ""},
		{"server before name", Routing{Clusters: []ClusterRoute{matching, {ParentInstance: "dev", Cluster: "in-cluster", Namespace: "argocd", Instance: "prod"}}}, "dev", "cluster", ""},
		{"explicit parent before cluster", Routing{Parents: []ParentRoute{{Parent: parent.ID, Instance: "prod"}}, Clusters: []ClusterRoute{matching}}, "prod", "parent", ""},
		{"cluster before repository and default", Routing{Clusters: []ClusterRoute{matching}, Repositories: []RepositoryRoute{{RepositoryURL: "https://github.com/example/charts.git", Project: "demo", Instance: "prod"}}, DefaultInstance: "prod"}, "dev", "cluster", ""},
		{"duplicate same target", Routing{Clusters: []ClusterRoute{matching, matching}}, "dev", "cluster", ""},
		{"conflicting scoped rules", Routing{Clusters: []ClusterRoute{matching, {ParentInstance: "dev", Cluster: matching.Cluster, Namespace: "argocd", Instance: "prod"}}, DefaultInstance: "dev"}, "", "", "routing_ambiguous"},
		{"conflicting legacy and scoped", Routing{Clusters: []ClusterRoute{matching, {Cluster: matching.Cluster, Namespace: "argocd", Instance: "prod"}}}, "", "", "routing_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rule, err := tc.routes.Route(parent, child)
			if tc.code == "" {
				if err != nil || got != tc.want || rule != tc.rule {
					t.Fatalf("route=%s/%s err=%v", got, rule, err)
				}
			} else {
				var failure *domain.Failure
				if got != "" || rule != "" || !errors.As(err, &failure) || failure.Issue.Code != tc.code {
					t.Fatalf("conflict was silently resolved: %v", err)
				}
			}
		})
	}
}

func TestParentInstanceProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name, selector string
		ok             bool
	}{
		{"configured", "      parentInstance: test\n", true},
		{"legacy", "", true},
		{"unknown", "      parentInstance: missing\n", false},
		{"typo", "      parentInstances: test\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := valid + "routing:\n  clusters:\n    - cluster: in-cluster\n      namespace: argocd\n      instance: test\n" + tc.selector
			_, err := Parse([]byte(input))
			if (err == nil) != tc.ok {
				t.Fatalf("valid=%v err=%v", tc.ok, err)
			}
		})
	}
}
