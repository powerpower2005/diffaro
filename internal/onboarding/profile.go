package onboarding

import (
	"fmt"
	"net/url"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
)

// Profile copies only the given connection and selected identities, never
// inline values, parameters or whole stored Application specs.
func Profile(instance profile.Instance, apps []domain.Application, set *domain.ApplicationID, gitAPI, gitTokenEnv string, singleInstance bool) (profile.Config, error) {
	if len(apps) == 0 {
		return profile.Config{}, fmt.Errorf("select at least one readable Application")
	}
	repository := domain.String(apps[0].Source(), "repoURL")
	if !profile.ValidEndpoint(repository) {
		return profile.Config{}, fmt.Errorf("source repository must be an HTTPS Git URL; configure a profile explicitly for unsupported repository URLs")
	}
	u, _ := url.Parse(repository)
	gitAPI = apiURL(gitAPI, u.Host)
	ids := []domain.ApplicationID{}
	for _, app := range apps {
		if err := preview.Supported(app); err != nil {
			return profile.Config{}, err
		}
		if !profile.SameRepository(repository, domain.String(app.Source(), "repoURL")) {
			return profile.Config{}, fmt.Errorf("selected Applications use multiple repositories; choose explicit roots and a primary repository in a reviewed profile")
		}
		ids = append(ids, app.ID)
	}
	c := newProfile(instance, profile.Git{APIURL: gitAPI, Repository: strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), RepositoryURL: repository, TokenEnv: gitTokenEnv}, singleInstance)
	if set != nil {
		c.ApplicationSets = []domain.ApplicationID{*set}
	} else {
		c.Applications = ids
	}
	return c, c.Validate()
}

// apiURL returns the explicit GitHub API URL, the GitHub.com API for a
// github.com repository host, or the GitHub Enterprise Server API path on
// any other host. init prints the URL it wrote.
func apiURL(gitAPI, host string) string {
	if gitAPI != "" {
		return gitAPI
	}
	if !strings.EqualFold(host, "github.com") {
		return "https://" + host + "/api/v3"
	}
	return "https://api.github.com"
}

// newProfile is a generated profile for one instance with default render
// limits and the starter run settings; with singleInstance, recursive
// children route to that instance.
func newProfile(instance profile.Instance, git profile.Git, singleInstance bool) profile.Config {
	c := profile.Config{Version: "v1alpha1", Git: git, Instances: []profile.Instance{instance}, Render: profile.DefaultRender(), Run: StarterRun()}
	if singleInstance {
		c.Routing.DefaultInstance = instance.ID
	}
	return c
}

// StarterRun is the run section init writes: a recursive terminal
// report by default, and a "ci" mode that saves PR Markdown, JSON, a trace
// and a private debug bundle quietly. The private bundle holds nothing the
// JSON report does not; both describe the infrastructure, so CI artifacts
// holding them must be readable by repository members only.
func StarterRun() profile.Run {
	yes := true
	return profile.Run{
		Defaults: profile.RunOptions{Format: "terminal", Recursive: &yes},
		Modes: map[string]profile.RunOptions{"ci": {
			Format: "pr-markdown", Output: "preview.md", JSONOutput: "preview.json", Trace: "trace.json",
			DebugBundle: "diffaro-bundle-private.tar.gz", DebugBundleLevel: "private", Quiet: &yes,
		}},
	}
}
