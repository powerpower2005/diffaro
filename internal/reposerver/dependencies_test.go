package reposerver

import (
	"reflect"
	"testing"
)

func TestHelmDependenciesPolicyCredentialsAndIsolation(t *testing.T) {
	k := &kubernetesContext{secrets: []map[string]string{
		{"_kind": "repository", "type": "helm", "url": "https://charts.example.com/team", "name": "team"},
		{"_kind": "repo-creds", "type": "helm", "url": "https://charts.example.com", "username": "fixture", "password": "fictional-password"},
		{"_kind": "repository", "type": "helm", "url": "registry.example.com/charts", "enableOCI": "true", "password": "fictional-oci"},
		{"_kind": "repository", "type": "helm", "url": "https://denied.example.com", "password": "must-not-appear"},
		{"_kind": "repository", "type": "helm", "url": "https://other.example.com", "project": "other", "password": "must-not-appear"},
		{"_kind": "repository", "type": "oci", "url": "registry.example.com/native", "password": "must-not-appear"},
	}}
	project := map[string]any{"sourceRepos": []any{"*", "!https://denied.example.com"}}
	repos, creds, err := k.helmDependencies(project, "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || len(creds) != 1 {
		t.Fatalf("unexpected dependency counts: %d %d", len(repos), len(creds))
	}
	repo := repos[0].(map[string]any)
	if repo["password"] != "fictional-password" || repo["name"] != "team" {
		t.Fatal("dependency credentials or alias were lost")
	}
	if repos[1].(map[string]any)["enableOCI"] != true {
		t.Fatal("Helm OCI setting was lost")
	}
	credential := creds[0].(map[string]any)
	if credential["url"] != "https://charts.example.com" || credential["repo"] != nil {
		t.Fatal("incorrect RepoCreds wire identity")
	}
	for i, j := 0, len(k.secrets)-1; i < j; i, j = i+1, j-1 {
		k.secrets[i], k.secrets[j] = k.secrets[j], k.secrets[i]
	}
	r2, c2, err := k.helmDependencies(project, "team")
	if err != nil || !reflect.DeepEqual(repos, r2) || !reflect.DeepEqual(creds, c2) {
		t.Fatal("Secret inventory ordering changed the request")
	}
}

func TestHelmDependenciesFailExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets []map[string]string
	}{
		{"duplicate", []map[string]string{{"_kind": "repository", "type": "helm", "url": "https://charts.example.com"}, {"_kind": "repository", "type": "helm", "url": "https://charts.example.com"}}},
		{"workload identity", []map[string]string{{"_kind": "repository", "type": "helm", "url": "https://charts.example.com", "useAzureWorkloadIdentity": "true"}}},
		{"scoped template", []map[string]string{{"_kind": "repo-creds", "type": "helm", "url": "https://charts.example.com", "project": "team"}}},
		{"repository-only field", []map[string]string{{"_kind": "repo-creds", "type": "helm", "url": "https://charts.example.com", "insecure": "true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &kubernetesContext{secrets: tc.secrets}
			if _, _, err := k.helmDependencies(map[string]any{"sourceRepos": []any{"*"}}, "team"); err == nil {
				t.Fatal("expected explicit failure")
			}
		})
	}
}
