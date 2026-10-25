package github

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"

	"diffaro/internal/domain"
)

// TreeEntry is one file of a repository tree at an immutable commit.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Link reports whether the entry is a symbolic link.
func (e TreeEntry) Link() bool { return e.Mode == "120000" }

// Tree lists every file of repository at revision. A truncated listing fails
// so callers can fall back to a full archive.
func (c *Client) Tree(ctx context.Context, repository, revision string) ([]TreeEntry, error) {
	if repoPath(repository) == "" || !ValidSHA(strings.ToLower(revision)) {
		return nil, domain.Fail(domain.CodeRevisionInvalid, "source", "tree requires a repository and immutable SHA", "resolve the source revision", nil)
	}
	var response struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := c.get(ctx, repoPath(repository)+"/git/trees/"+url.PathEscape(revision), url.Values{"recursive": {"1"}}, &response, repository, "source tree listing", "/repos/{owner}/{repo}/git/trees/{sha}", "Contents: read for the source repository"); err != nil {
		return nil, err
	}
	if response.Truncated {
		return nil, domain.Fail(domain.CodeLimitExceeded, "source", "source tree listing is truncated", "read the full source archive", nil)
	}
	entries := make([]TreeEntry, 0, len(response.Tree))
	for _, entry := range response.Tree {
		if entry.Type != "blob" {
			continue
		}
		if !domain.SafeRelativePath(entry.Path) || !ValidSHA(entry.SHA) {
			return nil, archiveFailure("source tree contains an unsafe path")
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Blob reads one file by its content SHA.
func (c *Client) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	if repoPath(repository) == "" || !ValidSHA(sha) {
		return nil, domain.Fail(domain.CodeRevisionInvalid, "source", "blob requires a repository and content SHA", "check the source tree", nil)
	}
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.get(ctx, repoPath(repository)+"/git/blobs/"+sha, nil, &response, repository, "source file read", "/repos/{owner}/{repo}/git/blobs/{sha}", "Contents: read for the source repository"); err != nil {
		return nil, err
	}
	if response.Encoding != "base64" {
		return nil, archiveFailure("source file encoding is unsupported")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, archiveFailure("source file content is invalid")
	}
	return data, nil
}

// InsideLink reports whether a link at name with target stays inside the
// repository, as Argo CD requires of symbolic links.
func InsideLink(name, target string) bool { return insideLink(name, target) }
