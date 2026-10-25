package onboarding

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"go.yaml.in/yaml/v3"
)

func TestProfileCopiesReferencesOnlyAndRequiresExplicitRouting(t *testing.T) {
	app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}, Spec: map[string]any{"source": map[string]any{"repoURL": "https://github.com/example/charts.git", "helm": map[string]any{"values": "password: secret-fixture"}}}}
	instance := profile.Instance{ID: "test", Endpoint: "https://argocd.example.invalid", TokenEnv: "ARGO_TOKEN"}
	c, err := Profile(instance, []domain.Application{app}, nil, "", "GIT_TOKEN", false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(c)
	if err != nil || c.Git.Repository != "example/charts" || c.Git.APIURL != "https://api.github.com" || len(c.Applications) != 1 || c.Routing.DefaultInstance != "" || strings.Contains(string(data), "secret-fixture") {
		t.Fatal("unsafe or invalid generated profile")
	}
	set := app.ID
	set.Name = "fleet"
	c, err = Profile(instance, []domain.Application{app}, &set, "", "GIT_TOKEN", true)
	if err != nil || len(c.ApplicationSets) != 1 || len(c.Applications) != 0 || c.Routing.DefaultInstance != "test" {
		t.Fatal("generated selection or explicit route lost")
	}
	app.Spec["source"].(map[string]any)["repoURL"] = "https://git.example.invalid/example/charts.git"
	if c, err = Profile(instance, []domain.Application{app}, nil, "", "GIT_TOKEN", false); err != nil || c.Git.APIURL != "https://git.example.invalid/api/v3" {
		t.Fatal("an enterprise host must get the GitHub Enterprise Server API path", err)
	}
	app.Spec["source"].(map[string]any)["repoURL"] = "https://GitHub.com/example/charts.git"
	if c, err = Profile(instance, []domain.Application{app}, nil, "", "GIT_TOKEN", false); err != nil || c.Git.APIURL != "https://api.github.com" {
		t.Fatal("host names are case-insensitive", err)
	}
	app.Spec["source"].(map[string]any)["repoURL"] = "https://git.example.invalid/example/charts.git"
	app.Spec["source"].(map[string]any)["plugin"] = map[string]any{"name": "untrusted"}
	if _, err = Profile(instance, []domain.Application{app}, nil, "https://git.example.invalid/api/v3", "GIT_TOKEN", false); err == nil {
		t.Fatal("unsupported source accepted")
	}
}
