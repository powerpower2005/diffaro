package profile

import (
	"diffaro/internal/domain"
	"errors"
	"strings"
	"testing"
)

func TestMultiSourceRoutingAndAmbiguity(t *testing.T) {
	parent := domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "root"}}
	child := domain.Application{ID: domain.ApplicationID{Namespace: "argocd", Name: "child"}, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://example.com/ignored"}, "sources": []any{map[string]any{"repoURL": "https://example.com/chart"}, map[string]any{"repoURL": "https://example.com/values", "ref": "values"}}}}
	r := Routing{Repositories: []RepositoryRoute{{RepositoryURL: "https://example.com/values", Project: "demo", Instance: "two"}, {RepositoryURL: "https://example.com/ignored", Project: "demo", Instance: "ignored"}}}
	instance, rule, err := r.Route(parent, child)
	if err != nil || instance != "two" || rule != "repository-project" {
		t.Fatalf("values-source routing failed: %s %v", instance, err)
	}
	r.Repositories = append(r.Repositories, RepositoryRoute{RepositoryURL: "https://example.com/chart", Project: "demo", Instance: "three"})
	_, _, err = r.Route(parent, child)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "routing_ambiguous" {
		t.Fatal("conflicting source routes were guessed")
	}
	r.Parents = []ParentRoute{{Parent: parent.ID, Instance: "one"}}
	instance, rule, err = r.Route(parent, child)
	if err != nil || instance != "one" || rule != "parent" {
		t.Fatal("explicit parent routing lost precedence")
	}
}

func TestRoutingFailureIdentifiesSelectorsWithoutGuessingInstance(t *testing.T) {
	parent := domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "root"}}
	child := domain.Application{ID: domain.ApplicationID{Namespace: "apps", Name: "child"}, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "workload-only"}}}
	for _, tc := range []struct {
		name        string
		destination map[string]any
		want        string
		setting     string
	}{
		{"name", map[string]any{"name": "management"}, `parent destination name="management"`, "routing.clusters"},
		{"server precedence", map[string]any{"server": "https://management.example.invalid", "name": "ignored"}, `parent destination server="https://management.example.invalid"`, "routing.clusters"},
		{"unresolved", nil, `parent destination unresolved=""`, "routing.parents"},
		{"credential URL", map[string]any{"server": "https://user:fictional-private-value@management.example.invalid"}, "unsafe server identifier omitted", "routing.parents"},
		{"query URL", map[string]any{"server": "https://management.example.invalid?credential=fictional-private-value"}, "unsafe server identifier omitted", "routing.parents"},
		{"fragment URL", map[string]any{"server": "https://management.example.invalid#fictional-private-value"}, "unsafe server identifier omitted", "routing.parents"},
		{"invalid URL", map[string]any{"server": "fictional-private-value"}, "unsafe server identifier omitted", "routing.parents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent.Spec = map[string]any{"destination": tc.destination}
			instance, rule, err := (Routing{}).Route(parent, child)
			var failure *domain.Failure
			if instance != "" || rule != "" || !errors.As(err, &failure) || failure.Issue.Code != "routing_missing" {
				t.Fatal("missing routing guessed a managing instance")
			}
			for _, text := range []string{tc.want, `parent="one/argocd/root"`, `child Application="apps/child"`, `child project="demo"`} {
				if !strings.Contains(failure.Issue.Reason, text) {
					t.Fatalf("missing selector %s: %s", text, failure.Issue.Reason)
				}
			}
			if !strings.Contains(failure.Issue.Action, tc.setting) || !strings.Contains(failure.Issue.Action, "only if all descendants") || !strings.Contains(failure.Issue.Action, "routing.defaultInstance") {
				t.Fatal("missing actionable configuration guidance or conditional default")
			}
			if tc.setting == "routing.clusters" && !strings.Contains(failure.Issue.Action, `namespace="apps"`) {
				t.Fatal("cluster selector lost the child Application namespace")
			}
			if strings.Contains(failure.Error()+failure.Issue.Action, "fictional-private-value") || strings.Contains(failure.Error(), "workload-only") {
				t.Fatal("diagnostic retained credentials or used the workload destination")
			}
		})
	}
}

func TestRoutingAmbiguityNamesPriorityAndSortedCandidates(t *testing.T) {
	parent := domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "root"}, Spec: map[string]any{"destination": map[string]any{"name": "management"}}}
	child := domain.Application{ID: domain.ApplicationID{Namespace: "apps", Name: "child"}}
	var previous string
	for _, instances := range [][]string{{"two", "one", "two"}, {"one", "two", "two"}} {
		r := Routing{DefaultInstance: "one"}
		for _, instance := range instances {
			r.Clusters = append(r.Clusters, ClusterRoute{Cluster: "management", Namespace: "apps", Instance: instance})
		}
		instance, rule, err := r.Route(parent, child)
		var failure *domain.Failure
		if instance != "" || rule != "" || !errors.As(err, &failure) || failure.Issue.Code != "routing_ambiguous" || !strings.Contains(failure.Issue.Reason, "routing.clusters at the same priority: one, two") || !strings.Contains(failure.Issue.Action, "lower-priority default cannot resolve") {
			t.Fatal("ambiguity was guessed or omitted the conflicting configuration")
		}
		if previous != "" && previous != failure.Error() {
			t.Fatal("candidate diagnostics depend on profile rule ordering")
		}
		previous = failure.Error()
	}
}

func TestRoutingPrecedenceAndManagingCluster(t *testing.T) {
	parent := domain.Application{ID: domain.ApplicationID{Instance: "parent", Namespace: "argocd", Name: "root"}, Spec: map[string]any{"destination": map[string]any{"name": "management", "namespace": "argocd"}}}
	child := domain.Application{ID: domain.ApplicationID{Namespace: "argocd", Name: "child"}, Spec: map[string]any{"source": map[string]any{"repoURL": "https://example.com/charts.git"}, "project": "demo", "destination": map[string]any{"name": "workloads"}}}
	r := Routing{Parents: []ParentRoute{{Parent: parent.ID, Instance: "parent-rule"}}, Clusters: []ClusterRoute{{Cluster: "management", Namespace: "argocd", Instance: "cluster-rule"}, {Cluster: "workloads", Namespace: "argocd", Instance: "wrong"}}, Repositories: []RepositoryRoute{{RepositoryURL: "https://example.com/charts", Project: "demo", Instance: "repo-rule"}}, DefaultInstance: "default"}
	for _, want := range []string{"parent-rule", "cluster-rule", "repo-rule", "default"} {
		got, _, err := r.Route(parent, child)
		if err != nil || got != want {
			t.Fatalf("wanted %s, got %s: %v", want, got, err)
		}
		switch want {
		case "parent-rule":
			r.Parents = nil
		case "cluster-rule":
			r.Clusters = nil
		case "repo-rule":
			r.Repositories = nil
		}
	}
	r.DefaultInstance = ""
	if _, _, err := r.Route(parent, child); err == nil {
		t.Fatal("missing route guessed an instance")
	}
	r.Clusters = []ClusterRoute{{Cluster: "management", Namespace: "argocd", Instance: "one"}, {Cluster: "management", Namespace: "argocd", Instance: "two"}}
	if _, _, err := r.Route(parent, child); err == nil {
		t.Fatal("ambiguous route selected an instance")
	}
}
