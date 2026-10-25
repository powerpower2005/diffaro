// Package github is the read-only Git provider adapter for the GitHub REST API:
// pull request revisions, immutable SHA resolution, changed files, file reads
// and source archives. Fork pull requests are rejected.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Client struct {
	HTTP *httpx.Client
	mu   sync.Mutex
	// pulls memoizes the head SHA read by PullRequest so ChangedFiles can
	// prove its listing belongs to the head this run renders.
	pulls map[string]pullState
}

type pullState struct {
	head         string
	changedFiles int
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any, repository, operation, route, permission string) error {
	return inRepository(c.HTTP.GetFor(ctx, path, query, out, diagnostic(operation, route, permission)), repository)
}

func diagnostic(operation, route, permission string) httpx.RequestDiagnostic {
	return httpx.RequestDiagnostic{
		Service: "GitHub", Operation: operation, Route: route, Permission: permission,
		AuthenticationAction: "confirm repository selection and organization approval/SSO authorization where required",
	}
}

// inRepository names repository on a failure, copying it so shared causes
// are not mutated.
func inRepository(err error, repository string) error {
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		return err
	}
	copy := *failure
	copy.Issue.Repository = repository
	return &copy
}

func ValidSHA(s string) bool { return shaPattern.MatchString(s) }
func validRef(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n\\?#") && !strings.Contains(s, "..") && !strings.HasPrefix(s, "/")
}
func repoPath(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return ""
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func (c *Client) Resolve(ctx context.Context, repo, ref string) (string, error) {
	if !validRef(ref) || repoPath(repo) == "" {
		return "", domain.Fail(domain.CodeRevisionInvalid, "revision", "invalid repository or Git reference", "use a repository-qualified branch, tag or commit SHA", nil)
	}
	var response struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, repoPath(repo)+"/commits/"+url.PathEscape(ref), nil, &response, repo, "revision lookup", "/repos/{owner}/{repo}/commits/{ref}", "Contents: read and Metadata: read for the selected repository"); err != nil {
		return "", unresolvedRef(err, ref)
	}
	if !ValidSHA(response.SHA) {
		return "", domain.Fail(domain.CodeRevisionInvalid, "revision", "provider did not return an immutable Git SHA", "check provider compatibility and revision visibility", nil)
	}
	return response.SHA, nil
}

// unresolvedRef reports GitHub's 422 for a branch, tag or SHA that names no
// commit, such as a deleted branch, as a missing revision rather than a
// backend fault.
func unresolvedRef(err error, ref string) error {
	var failure *domain.Failure
	var status *httpx.StatusError
	if !errors.As(err, &failure) || !errors.As(err, &status) || status.StatusCode != http.StatusUnprocessableEntity {
		return err
	}
	copy := *failure
	copy.Issue.Code, copy.Issue.ReasonCode, copy.Issue.Stage, copy.Issue.Revision = domain.CodeNotFound, domain.ReasonRevisionMissing, "revision", ref
	copy.Issue.Reason = "GitHub revision lookup: Git reference " + strconv.Quote(ref) + " names no commit in the repository (HTTP 422)"
	copy.Issue.Action = "check that the branch, tag or commit exists in the repository; restore a deleted branch or update the targetRevision"
	return &copy
}

func (c *Client) PullRequest(ctx context.Context, repo string, number int) (domain.RevisionPair, error) {
	if number <= 0 || repoPath(repo) == "" {
		return domain.RevisionPair{}, domain.Fail(domain.CodeRevisionInvalid, "revision", "invalid pull request input", "provide a positive PR number", nil)
	}
	var pr struct {
		Base struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		ChangedFiles int `json:"changed_files"`
	}
	if err := c.get(ctx, repoPath(repo)+"/pulls/"+strconv.Itoa(number), nil, &pr, repo, "pull request lookup", "/repos/{owner}/{repo}/pulls/{number}", "Pull requests: read and Metadata: read for the selected repository"); err != nil {
		return domain.RevisionPair{}, err
	}
	c.mu.Lock()
	if c.pulls == nil {
		c.pulls = map[string]pullState{}
	}
	c.pulls[pullKey(repo, number)] = pullState{head: pr.Head.SHA, changedFiles: pr.ChangedFiles}
	c.mu.Unlock()
	if pr.Base.Repo.FullName != repo || pr.Head.Repo.FullName != repo {
		return domain.RevisionPair{}, domain.Fail(domain.CodeUntrustedFork, "revision", "cross-repository pull requests require an isolated render policy", "use a reviewed same-repository branch until fork isolation is configured", nil)
	}
	if !ValidSHA(pr.Base.SHA) || !ValidSHA(pr.Head.SHA) {
		return domain.RevisionPair{}, domain.Fail(domain.CodeRevisionInvalid, "revision", "PR metadata did not contain immutable SHAs", "refresh provider metadata", nil)
	}
	var comparison struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := c.get(ctx, repoPath(repo)+"/compare/"+pr.Base.SHA+"..."+pr.Head.SHA, url.Values{"per_page": {"1"}}, &comparison, repo, "merge-base lookup", "/repos/{owner}/{repo}/compare/{base}...{head}", "Contents: read and Metadata: read for the selected repository"); err != nil {
		return domain.RevisionPair{}, err
	}
	if !ValidSHA(comparison.MergeBase.SHA) {
		return domain.RevisionPair{}, domain.Fail(domain.CodeRevisionInvalid, "revision", "provider did not return a merge-base SHA", "ensure the PR shares history with its base", nil)
	}
	return domain.RevisionPair{Repository: repo, Base: comparison.MergeBase.SHA, Head: pr.Head.SHA, Mode: domain.RevisionModePR}, nil
}

func pullKey(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

// maxPullRequestFiles is GitHub's documented cap for the PR files listing.
const maxPullRequestFiles = 3000

// ChangedFiles lists the paths a pull request changes, including the previous
// path of renamed files. states holds StateAdded or StateRemoved for the paths
// the PR adds or removes; a rename adds its new path and removes the previous
// one. GitHub lists the files of the PR's current head, so the pull request is
// read again after listing. complete is false when GitHub truncates the
// listing, the listed count differs from the PR's changed_files total, or the
// head moved away from the one PullRequest resolved for this run; callers must
// then treat every candidate as possibly affected.
func (c *Client) ChangedFiles(ctx context.Context, repo string, number int) (paths []string, states map[string]domain.State, complete bool, err error) {
	if number <= 0 || repoPath(repo) == "" {
		return nil, nil, false, domain.Fail(domain.CodeRevisionInvalid, "revision", "invalid pull request input", "provide a positive PR number", nil)
	}
	base := repoPath(repo) + "/pulls/" + strconv.Itoa(number)
	c.mu.Lock()
	before, known := c.pulls[pullKey(repo, number)]
	c.mu.Unlock()
	if !known {
		if before, err = c.pullState(ctx, repo, base); err != nil {
			return nil, nil, false, err
		}
	}
	paths, states, listed := []string{}, map[string]domain.State{}, 0
	for page := 1; listed < maxPullRequestFiles; page++ {
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
		}
		if err := c.get(ctx, base+"/files", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, &files, repo, "pull request files lookup", "/repos/{owner}/{repo}/pulls/{number}/files", "Pull requests: read and Metadata: read for the selected repository"); err != nil {
			return nil, nil, false, err
		}
		for _, f := range files {
			paths = append(paths, f.Filename)
			if f.PreviousFilename != "" {
				paths = append(paths, f.PreviousFilename)
			}
			switch f.Status {
			case "added", "copied":
				states[f.Filename] = domain.StateAdded
			case "renamed":
				states[f.Filename] = domain.StateAdded
				if f.PreviousFilename != "" {
					states[f.PreviousFilename] = domain.StateRemoved
				}
			case "removed":
				states[f.Filename] = domain.StateRemoved
			}
		}
		listed += len(files)
		if len(files) < 100 {
			break
		}
	}
	after, err := c.pullState(ctx, repo, base)
	if err != nil {
		return nil, nil, false, err
	}
	return paths, states, after.head == before.head && listed == after.changedFiles && listed < maxPullRequestFiles, nil
}

func (c *Client) pullState(ctx context.Context, repo, base string) (pullState, error) {
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		ChangedFiles int `json:"changed_files"`
	}
	if err := c.get(ctx, base, nil, &pr, repo, "pull request lookup", "/repos/{owner}/{repo}/pulls/{number}", "Pull requests: read and Metadata: read for the selected repository"); err != nil {
		return pullState{}, err
	}
	return pullState{head: pr.Head.SHA, changedFiles: pr.ChangedFiles}, nil
}

// File reads one repository file at an immutable revision through the
// contents API. found is false when the file does not exist at that revision.
func (c *Client) File(ctx context.Context, repo, revision, file string) ([]byte, bool, error) {
	if repoPath(repo) == "" || !ValidSHA(revision) || !domain.SafeRelativePathAllowingColon(file) {
		return nil, false, domain.Fail(domain.CodeRequestInvalid, "source", "invalid repository file request", "use a relative repository path and immutable revision", nil)
	}
	segments := strings.Split(file, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	var content struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	err := c.get(ctx, repoPath(repo)+"/contents/"+strings.Join(segments, "/"), url.Values{"ref": {revision}}, &content, repo, "repository file lookup", "/repos/{owner}/{repo}/contents/{path}", "Contents: read for the selected repository")
	var failure *domain.Failure
	if errors.As(err, &failure) && failure.Issue.Code == domain.CodeNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if content.Type != "file" || content.Encoding != "base64" {
		return nil, false, domain.Fail(domain.CodeResponseInvalid, "source", "repository path is not a readable file", "check the repository path", nil)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return nil, false, domain.Fail(domain.CodeResponseInvalid, "source", "repository file content is not valid base64", "check provider compatibility", nil)
	}
	return data, true, nil
}
