package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// Archive reads immutable source files. Credentials are never forwarded to an
// archive redirect, and links/devices/submodules are not extracted or executed.
// Symbolic links that stay inside the repository are returned as paths only
// (links); links that escape it fail the archive.
func (c *Client) Archive(ctx context.Context, repository, revision string) (files map[string][]byte, links []string, err error) {
	const route = "/repos/{owner}/{repo}/tarball/{ref}"
	status := 0
	end := domain.TraceHTTP(ctx, "GitHub", "source archive download", route)
	defer func() { end(status, err) }()
	if repoPath(repository) == "" || !ValidSHA(strings.ToLower(revision)) {
		return nil, nil, domain.Fail(domain.CodeRevisionInvalid, "source", "archive requires a repository and immutable SHA", "resolve the source revision", nil)
	}
	request := diagnostic("source archive download", route, "Contents: read for the source repository")
	response, err := c.HTTP.OpenFor(ctx, repoPath(repository)+"/tarball/"+revision, "application/vnd.github+json", request)
	if err != nil {
		status = httpx.ResponseStatus(err)
		return nil, nil, inRepository(err, repository)
	}
	status = response.StatusCode
	if response.StatusCode == http.StatusFound || response.StatusCode == http.StatusTemporaryRedirect {
		location, e := response.Location()
		response.Body.Close()
		origin, _ := url.Parse(c.HTTP.Base)
		if e != nil || location.Scheme != "https" || location.User != nil || (location.Host != origin.Host && !(origin.Host == "api.github.com" && location.Host == "codeload.github.com")) {
			return nil, nil, archiveFailure("untrusted archive redirect")
		}
		// Signed download URLs require no credential; OpenRedirectFor sends none.
		request.Operation, request.Route = "source archive redirect download", route+" redirect"
		response, err = c.HTTP.OpenRedirectFor(ctx, location, "", request)
		if err != nil {
			status = httpx.ResponseStatus(err)
			return nil, nil, inRepository(err, repository)
		}
		status = response.StatusCode
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, archiveFailure("GitHub did not return a source archive")
	}
	return readArchive(response.Body, c.HTTP.Limit, true)
}

// ReadArchive bounds compressed and expanded data and rejects filesystem escape.
// Repository files remain in memory; inline values and credentials are not cached on disk.
func ReadArchive(input io.Reader, limit int64) (map[string][]byte, error) {
	files, _, err := readArchive(input, limit, false)
	return files, err
}

// readArchive reads a gzip tar archive. With allowLinks, symbolic links whose
// target stays inside the archive root are recorded by path instead of failing.
func readArchive(input io.Reader, limit int64, allowLinks bool) (map[string][]byte, []string, error) {
	compressed := &io.LimitedReader{R: input, N: limit + 1}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, nil, archiveFailure("source archive is not gzip")
	}
	defer gz.Close()
	// Bound tar padding and metadata too, including extension headers which
	// archive/tar consumes internally before returning a regular header.
	expanded := &io.LimitedReader{R: gz, N: limit + (16 << 20) + 1}
	tr := tar.NewReader(expanded)
	files := map[string][]byte{}
	var links []string
	var used int64
	root := ""
	for count := 0; ; count++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			if compressed.N <= 0 {
				return nil, nil, ArchiveBytesError(limit+1, limit, true)
			}
			return nil, nil, archiveFailure("source archive is invalid")
		}
		if count >= MaxArchiveEntries {
			return nil, nil, ArchiveEntriesError()
		}
		if h.Size < 0 {
			return nil, nil, archiveFailure("source archive is invalid")
		}
		if h.Size > limit-used {
			return nil, nil, ArchiveBytesError(used+h.Size, limit, true)
		}
		// git archive emits a PAX global commit comment before its root directory.
		// It is metadata only, never a filesystem entry or a source file.
		if h.Typeflag == tar.TypeXGlobalHeader {
			used += h.Size
			continue
		}
		parts := strings.SplitN(h.Name, "/", 2)
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root || root == "" || root == "." || root == ".." || strings.ContainsAny(h.Name, "\\:\x00") || strings.HasPrefix(h.Name, "/") {
			return nil, nil, archiveFailure("source archive contains an unsafe path")
		}
		if h.Typeflag == tar.TypeDir {
			clean := strings.TrimSuffix(h.Name, "/")
			if clean != path.Clean(clean) || root == "." || root == ".." {
				return nil, nil, archiveFailure("source archive contains an unsafe directory")
			}
			continue
		}
		if allowLinks && h.Typeflag == tar.TypeSymlink && len(parts) == 2 && parts[1] == path.Clean(parts[1]) && insideLink(parts[1], h.Linkname) {
			links = append(links, parts[1])
			continue
		}
		if len(parts) != 2 || h.Typeflag != tar.TypeReg || parts[1] != path.Clean(parts[1]) || parts[1] == ".." || strings.HasPrefix(parts[1], "../") {
			return nil, nil, archiveFailure("source archive links or unsafe paths are unsupported")
		}
		if _, exists := files[parts[1]]; exists {
			return nil, nil, archiveFailure("source archive contains duplicate files")
		}
		data, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil || int64(len(data)) != h.Size {
			if compressed.N <= 0 {
				return nil, nil, ArchiveBytesError(limit+1, limit, true)
			}
			return nil, nil, archiveFailure("source archive file is truncated")
		}
		used += h.Size
		files[parts[1]] = data
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil || expanded.N <= 0 {
		return nil, nil, archiveFailure("source archive expanded metadata or checksum is invalid")
	}
	if compressed.N <= 0 {
		return nil, nil, archiveFailure("compressed archive exceeds byte limit")
	}
	return files, links, nil
}

// MaxArchiveEntries bounds the entries of one source archive.
const MaxArchiveEntries = 10000

// ArchiveBytesError is the failure of a source archive over its byte bound,
// render.maxResponseBytes. lowerBound means reading stopped at the bound.
func ArchiveBytesError(observed, limit int64, lowerBound bool) error {
	return domain.LimitError(domain.CodeLimitExceeded, "source", "source archive", "render.maxResponseBytes", observed, limit, "bytes", lowerBound, domain.MaxRenderResponseBytes)
}

// ArchiveEntriesError is the failure of a source archive with more than
// MaxArchiveEntries entries; that bound is fixed.
func ArchiveEntriesError() error {
	return domain.Fail(domain.CodeLimitExceeded, "source", fmt.Sprintf("source archive has more than %d entries", MaxArchiveEntries), "this entry limit is fixed; reduce the number of files in the repository or narrow the analysis scope", nil)
}

func archiveFailure(reason string) error {
	return domain.Fail(domain.CodeSourceArchiveFailed, "source", reason, "check repository access, archive limits and supported regular files", nil)
}

// insideLink reports whether a link at name with target stays inside the
// repository, as Argo CD requires of symbolic links.
func insideLink(name, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\\:\x00") {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}
