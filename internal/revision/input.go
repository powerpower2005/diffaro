// Package revision resolves base and head inputs into immutable SHAs (Resolve,
// PullRequestNumber) and plans per-source revisions for multi-source
// Applications (PlanSources).
package revision

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// PullRequestNumber accepts a number or a PR URL for the configured repository.
// It never changes the API endpoint or forwards credentials to an input URL.
func PullRequestNumber(input, repositoryURL string) (int, error) {
	if input == "" {
		return 0, nil
	}
	invalid := fmt.Errorf("--pr must be a positive number or a pull request URL for the configured repository")
	number := input
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		r, repoErr := url.Parse(repositoryURL)
		if err != nil || repoErr != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.EqualFold(u.Host, r.Host) {
			return 0, invalid
		}
		prefix := strings.TrimSuffix(strings.TrimSuffix(r.Path, "/"), ".git") + "/pull/"
		if !strings.HasPrefix(u.Path, prefix) {
			return 0, invalid
		}
		number = strings.TrimPrefix(u.Path, prefix)
	}
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return 0, invalid
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return 0, invalid
	}
	return n, nil
}
