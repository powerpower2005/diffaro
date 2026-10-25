package preview

import (
	"fmt"
	"slices"
	"sort"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// OtherHostSources counts, per source URL, the Applications that use the PR
// repository's path on a host the profile does not treat as the PR
// repository. Such Applications are not selected; when the host is a mirror,
// the profile is missing a mirrorHosts or repositoryAliases entry.
func OtherHostSources(apps []domain.Application, git profile.Git) map[string]int {
	counts := map[string]int{}
	for _, app := range apps {
		for _, source := range otherHostSources(app, git.RepositoryURL, git.Aliases()) {
			counts[source]++
		}
	}
	return counts
}

// RepositoryApplications counts the Applications that use the PR repository
// under its URL, an alias or a mirror host, as discovery selects them.
func RepositoryApplications(apps []domain.Application, git profile.Git) int {
	n := 0
	for _, app := range apps {
		if referencesRepository(app, git.RepositoryURL, git.Aliases()...) {
			n++
		}
	}
	return n
}

// otherHostSources lists the distinct source URLs of app that have the PR
// repository's path on another host, unless app already uses the PR
// repository.
func otherHostSources(app domain.Application, repository string, aliases []string) []string {
	if referencesRepository(app, repository, aliases...) {
		return nil
	}
	var out []string
	for _, url := range sourceRepositories(app) {
		if other, ok := profile.SamePathOtherHost(url, repository); ok && !slices.Contains(out, other) {
			out = append(out, other)
		}
	}
	return out
}

// OtherHostHint states the unselected Applications on other hosts and the
// setting that would select them, or "" when there are none. The setting
// comes early so a shortened PR Markdown line still shows it.
func OtherHostHint(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	urls := make([]string, 0, len(counts))
	total := 0
	for url, n := range counts {
		urls = append(urls, url)
		total += n
	}
	sort.Strings(urls)
	more := ""
	if len(urls) > 1 {
		more = fmt.Sprintf(" (and %d more URLs)", len(urls)-1)
	}
	return fmt.Sprintf("%d Application(s) read the PR repository from %s%s and were not selected; add that host to git.mirrorHosts (init --mirror-host) if it mirrors the PR repository, or the URL to git.repositoryAliases (init --repository-alias).", total, urls[0], more)
}
