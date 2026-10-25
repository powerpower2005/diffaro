package onboarding

import (
	"fmt"
	"net/url"
	"strings"

	"diffaro/internal/profile"
	"diffaro/internal/revision"
)

// DiscoveryProfile derives only repository identity from the PR URL. Credentials
// remain environment references and the URL never selects the Argo endpoint.
func DiscoveryProfile(instance profile.Instance, prURL, gitAPI, gitTokenEnv string, singleInstance bool) (profile.Config, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return profile.Config{}, fmt.Errorf("init --pr requires an HTTPS pull request URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return profile.Config{}, fmt.Errorf("init --pr requires an HTTPS pull request URL")
	}
	repository := "https://" + u.Host + "/" + parts[0] + "/" + parts[1]
	if _, err := revision.PullRequestNumber(prURL, repository); err != nil {
		return profile.Config{}, err
	}
	gitAPI = apiURL(gitAPI, u.Host)
	c := newProfile(instance, profile.Git{APIURL: gitAPI, Repository: parts[0] + "/" + parts[1], RepositoryURL: repository, TokenEnv: gitTokenEnv}, singleInstance)
	// Changed-path selection renders only the candidates the PR's files can
	// affect and is the generated default; init --selection all
	// opts out.
	c.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	return c, c.Validate()
}
