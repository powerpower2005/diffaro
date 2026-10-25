package localhelm

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/github"
	"diffaro/internal/gitsource"
)

const (
	// Repositories whose files total at most this many bytes, and at most
	// render.maxResponseBytes, are read as one archive; larger ones are read
	// file by file.
	sparseThreshold = 8 << 20
	maxSparseLinks  = 64
	blobConcurrency = 8
	// Once renders of one repository revision need more files than this in
	// total, the revision is read as an archive when the whole repository
	// fits the archive and snapshot limits: one request instead of one per
	// file.
	maxSparseBlobs = 64
	// maxArchiveEntries keeps archive files and directories under the
	// archive reader's github.MaxArchiveEntries bound.
	maxArchiveEntries = gitsource.MaxArchiveEntries
)

// sourceNeeds lists what renders read from one repository revision: whole
// directories (chart roots and file:// dependencies) and single files.
type sourceNeeds struct {
	dirs  map[string]bool
	files map[string]bool
}

func newSourceNeeds() *sourceNeeds {
	return &sourceNeeds{dirs: map[string]bool{}, files: map[string]bool{}}
}

// planNeeds computes the paths each repository revision of the request's
// source plan must provide, keyed like Request.Repositories. Value files and
// file parameters resolve like collectValues (resolveInputs); one that is
// malformed or cannot resolve fails the render before any file is read, so it
// needs none.
func planNeeds(req Request) map[string]*sourceNeeds {
	needs := map[string]*sourceNeeds{}
	get := func(key string) *sourceNeeds {
		if needs[key] == nil {
			needs[key] = newSourceNeeds()
		}
		return needs[key]
	}
	for _, s := range req.Plan {
		need := get(s.Repository + "@" + s.Revision)
		if root := domain.String(s.Source, "path"); root != "" {
			need.dirs[path.Clean(root)] = true
		}
	}
	for _, in := range resolveInputs(req, planRefs(req.Plan)) {
		if in.Malformed || in.Raw == "" || in.Err != nil {
			continue
		}
		get(in.RepoKey).files[in.Path] = true
	}
	return needs
}

// sourceFiles returns the files and in-repository links that renders need
// from repository at sha. It reads only the needed files from large
// repositories and falls back to the full archive otherwise.
func (c *Client) sourceFiles(ctx context.Context, repository, sha string, needs *sourceNeeds) (map[string][]byte, []string, error) {
	_, ok := c.Git.(gitsource.TreeReader)
	if !ok || needs == nil || needs.dirs["."] {
		return c.archive(ctx, repository, sha)
	}
	entries, err := c.sources().Tree(ctx, repository, sha)
	if err != nil {
		// A truncated listing or a provider without the trees API still
		// serves the archive. Other failures, such as rate limits or server
		// errors, are reported as they are rather than as archive limits.
		var failure *domain.Failure
		if errors.As(err, &failure) && (failure.Issue.Code == domain.CodeNotFound || failure.Issue.Code == domain.CodeLimitExceeded) {
			return c.archive(ctx, repository, sha)
		}
		return nil, nil, err
	}
	var total int64
	var linkEntries []github.TreeEntry
	byPath := make(map[string]github.TreeEntry, len(entries))
	for _, entry := range entries {
		total += entry.Size
		byPath[entry.Path] = entry
		if entry.Link() {
			linkEntries = append(linkEntries, entry)
		}
	}
	// A small repository is read as one archive only when it also fits the
	// archive response bound (render.maxResponseBytes); a lower bound reads it
	// file by file. An archive another step of the run already read, such as
	// changed-path selection, serves the render without requests when it fits.
	small := total <= sparseThreshold && (c.Limit <= 0 || total <= c.Limit)
	if small || len(linkEntries) > maxSparseLinks || c.archived(repository, sha) || (c.sources().HoldsArchive(repository, sha) && c.heldArchiveFits(total, len(entries))) {
		return c.archive(ctx, repository, sha)
	}
	needed := 0
	for _, entry := range entries {
		if !entry.Link() && (needs.files[entry.Path] || underAny(entry.Path, needs.dirs)) {
			needed++
		}
	}
	if !c.planSparse(repository+"@"+sha, needed) && c.archiveFits(total, len(entries)) {
		return c.archive(ctx, repository, sha)
	}
	// Like the archive path and Argo CD, a link escaping the repository fails.
	links := make([]string, 0, len(linkEntries))
	for _, entry := range linkEntries {
		target, err := c.blob(ctx, repository, entry)
		if err != nil {
			return nil, nil, err
		}
		if !github.InsideLink(entry.Path, string(target)) {
			return nil, nil, domain.Fail(domain.CodeSourceArchiveFailed, "source", "source archive links or unsafe paths are unsupported", "check repository access, archive limits and supported regular files", nil)
		}
		links = append(links, entry.Path)
	}
	files, err := c.readNeeded(ctx, repository, entries, needs)
	if err != nil {
		return nil, nil, err
	}
	return files, links, nil
}

// readNeeded reads the needed files of entries file by file, adding the
// directories of same-repository file:// dependencies as their charts are
// read, up to 16 levels deep.
func (c *Client) readNeeded(ctx context.Context, repository string, entries []github.TreeEntry, needs *sourceNeeds) (map[string][]byte, error) {
	files := map[string][]byte{}
	dirs := map[string]bool{}
	for dir := range needs.dirs {
		dirs[dir] = true
	}
	loaded := map[string]bool{}
	for depth := 0; depth <= 16; depth++ {
		var pending []github.TreeEntry
		for _, entry := range entries {
			if loaded[entry.Path] || entry.Link() || !(needs.files[entry.Path] || underAny(entry.Path, dirs)) {
				continue
			}
			loaded[entry.Path] = true
			pending = append(pending, entry)
		}
		if err := c.readBlobs(ctx, repository, pending, files); err != nil {
			return nil, err
		}
		added := false
		for dir := range dirs {
			// A chart file that does not parse adds no directory here; the
			// render then reports the chart itself.
			dependencies, _ := fileDependencies(files, dir, false)
			for _, dependency := range dependencies {
				if !dirs[dependency] && domain.SafeRelativePath(dependency) {
					dirs[dependency] = true
					added = true
				}
			}
		}
		if !added {
			return files, nil
		}
	}
	return nil, renderError()
}

func underAny(name string, dirs map[string]bool) bool {
	for dir := range dirs {
		if strings.HasPrefix(name, dir+"/") {
			return true
		}
	}
	return false
}

// blob reads one file, cached by repository and content SHA and counted once against the
// instance source snapshot limit.
// Concurrent reads of the same SHA, such as base and head, share one request.
func (c *Client) blob(ctx context.Context, repository string, entry github.TreeEntry) ([]byte, error) {
	data, err := c.sources().Blob(ctx, repository, entry.SHA)
	if err != nil {
		return nil, err
	}
	if err := c.charge("blob:"+entry.SHA, int64(len(data))); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) readBlobs(ctx context.Context, repository string, entries []github.TreeEntry, files map[string][]byte) error {
	results := make([][]byte, len(entries))
	errs := make([]error, len(entries))
	slots := make(chan struct{}, blobConcurrency)
	var wg sync.WaitGroup
	for i, entry := range entries {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			// A panic fails this source read only.
			defer domain.RecoverAs(&errs[i], "source")
			results[i], errs[i] = c.blob(ctx, repository, entry)
		}()
	}
	wg.Wait()
	for i, entry := range entries {
		if errs[i] != nil {
			return errs[i]
		}
		files[entry.Path] = results[i]
	}
	return nil
}

// archived reports whether this repository revision is already held as an
// archive, which then serves every later render without requests.
func (c *Client) archived(repository, sha string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.charged["archive:"+repository+"@"+sha]
}

// planSparse adds needed files to the revision's file-by-file budget and
// reports whether they stay within it.
func (c *Client) planSparse(key string, needed int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sparse == nil {
		c.sparse = map[string]int{}
	}
	if c.sparse[key]+needed > maxSparseBlobs {
		return false
	}
	c.sparse[key] += needed
	return true
}

// heldArchiveFits reports whether an archive the run already read fits the
// archive entry bound and this instance's remaining source snapshot budget;
// the response bound applied when it was read.
func (c *Client) heldArchiveFits(total int64, entries int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return entries <= maxArchiveEntries && c.bytes+total <= c.snapshotLimit()
}

// archiveFits reports whether the whole repository fits the archive response
// bound, the archive entry bound and the remaining source snapshot budget.
func (c *Client) archiveFits(total int64, entries int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Limit > 0 && total <= c.Limit && entries <= maxArchiveEntries && c.bytes+total <= c.snapshotLimit()
}
