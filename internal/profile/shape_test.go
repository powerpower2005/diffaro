package profile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// A profile shape names backend kinds, features and changed limits only;
// none of the profile's hosts, repositories, names or values appear.
func TestShapeNamesSettingsWithoutValues(t *testing.T) {
	c := Config{
		Git:             Git{RepositoryURL: "https://git.fictional-host.example/org/charts.git", MirrorHosts: []string{"mirror.fictional-host.example"}, LocalRepositories: []LocalRepository{{Name: "org/charts", Path: "/srv/fictional-cache"}}},
		Instances:       []Instance{{ID: "fictional-east", Endpoint: "https://argocd.fictional-host.example"}, {ID: "fictional-west", LocalHelm: true, HelmBinary: "/opt/fictional/helm", HelmEnvironment: []HelmEnvVar{{Name: "FICTIONAL_ENDPOINT", Value: "fictional-value"}}}, {ID: "fictional-repo", RepoServer: &RepoServer{}}},
		Discovery:       &Discovery{Mode: "repository", Selection: SelectionChangedPaths},
		Render:          DefaultRender(),
		SensitivePaths:  []string{"/spec/fictional-path"},
		Failures:        Failures{ExcludePreexistingFromExit: true},
		Policies:        []domain.PolicyRule{{ID: "fictional-policy"}},
		ApplicationSets: []domain.ApplicationID{{Name: "fictional-set"}},
	}
	c.Render.MaxConcurrency = 8
	shape := c.Shape()
	if shape.Instances != 3 || !slices.Equal(shape.Backends, []string{"argocd-application-api", "argocd-repo-server", "local-helm-external"}) || shape.Selection != SelectionChangedPaths {
		t.Fatalf("shape = %+v", shape)
	}
	if want := []string{"applicationSets", "excludePreexistingFromExit", "helmEnvironment", "localRepositories", "mirrorHosts", "policies", "repoServer", "sensitivePaths"}; !slices.Equal(shape.Features, want) {
		t.Fatalf("features = %v, want %v", shape.Features, want)
	}
	if !slices.Equal(shape.ChangedLimits, []string{"render.maxConcurrency"}) {
		t.Fatalf("changed limits = %v", shape.ChangedLimits)
	}
	encoded, _ := json.Marshal(shape)
	if strings.Contains(string(encoded), "fictional") {
		t.Fatalf("shape holds profile values: %s", encoded)
	}
	if plain := (Config{Render: DefaultRender(), Instances: []Instance{{LocalHelm: true}}}).Shape(); plain.Selection != "" || len(plain.Features) != 0 || len(plain.ChangedLimits) != 0 || !slices.Equal(plain.Backends, []string{"local-helm"}) {
		t.Fatalf("default shape = %+v", plain)
	}
	if all := (Config{Render: DefaultRender(), Discovery: &Discovery{Mode: "repository"}}).Shape(); all.Selection != "all" {
		t.Fatalf("discovery without selection = %q, want all", all.Selection)
	}
}
