package onboarding

import (
	"testing"

	"diffaro/internal/profile"
)

func TestDiscoveryProfileFromPR(t *testing.T) {
	instance := profile.Instance{ID: "test", Endpoint: "https://argocd.example.com", TokenEnv: "ARGO_TOKEN"}
	for _, single := range []bool{false, true} {
		c, err := DiscoveryProfile(instance, "https://github.com/example/charts/pull/12", "", "GIT_TOKEN", single)
		if err != nil || c.Discovery == nil || c.Discovery.Mode != "repository" || len(c.Applications) != 0 || c.Git.Repository != "example/charts" || c.Git.APIURL != "https://api.github.com" {
			t.Fatalf("unexpected profile: %+v, %v", c, err)
		}
		if (c.Routing.DefaultInstance == "test") != single {
			t.Fatal("routing must follow explicit single-instance selection")
		}
	}
	for _, input := range []string{"12", "https://github.com/example/charts/pull/0", "http://github.com/example/charts/pull/12", "https://token@github.com/example/charts/pull/12", "https://github.com/example/charts/pull/12?token=secret", "https://github.com/example/charts/pull/12/files"} {
		if _, err := DiscoveryProfile(instance, input, "", "GIT_TOKEN", false); err == nil {
			t.Fatalf("invalid or unconfigured URL accepted: %s", input)
		}
	}
	if c, err := DiscoveryProfile(instance, "https://git.example.com/example/charts/pull/12", "", "GIT_TOKEN", false); err != nil || c.Git.APIURL != "https://git.example.com/api/v3" {
		t.Fatal("an enterprise host must get the GitHub Enterprise Server API path", c.Git.APIURL, err)
	}
	if c, err := DiscoveryProfile(instance, "https://git.example.com/example/charts/pull/12", "https://api.git.example.com", "GIT_TOKEN", false); err != nil || c.Git.APIURL != "https://api.git.example.com" {
		t.Fatal("an explicit --git-api must win over the derived path", c.Git.APIURL, err)
	}
}
