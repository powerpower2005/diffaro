// Package gitsource caches immutable repository content for selection and rendering.
package gitsource

import (
	"context"
	"reflect"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/fetchcache"
	"diffaro/internal/github"
)

type archiveContent struct {
	files map[string][]byte
	links []string
}

// Cache holds GitHub source content read during one run so that every
// Argo CD instance rendering the same repository revision reads it once.
//
// Sharing across instances is credential-safe because every key is immutable
// and the cached bytes are a pure function of it: archives and trees are keyed
// by (repository, commit SHA) and blobs by (repository, content SHA). A cache
// is bound to the one Git reader (one token and host) that filled it, and
// consumers check ReadsWith before sharing it with another reader.
// The cache never authorizes a read: renderers still check their own AppProject
// policy and repository host, and charge their own snapshot byte limits.
type Cache struct {
	reader Reader
	// limit bounds the bytes retained here; zero means unlimited. Content
	// beyond it is returned to its callers but not kept.
	limit    int64
	mu       sync.Mutex
	bytes    int64
	archives map[string]*fetchcache.Entry[archiveContent]
	trees    map[string]*fetchcache.Entry[[]github.TreeEntry]
	blobs    map[string]*fetchcache.Entry[[]byte]
	// held maps a repository to blob SHAs and their paths in the archives
	// this cache keeps, so a blob of another revision with the same content
	// is served without a request (blobFromArchive).
	held map[string]map[string]heldBlob
}

// heldBlob locates a blob in a kept archive.
type heldBlob struct {
	revision, path string
}

// New returns a run-scoped cache for content read through reader.
func New(reader Reader, limit int64) *Cache {
	return &Cache{reader: reader, limit: limit, archives: map[string]*fetchcache.Entry[archiveContent]{}, trees: map[string]*fetchcache.Entry[[]github.TreeEntry]{}, blobs: map[string]*fetchcache.Entry[[]byte]{}}
}

// ReadsWith reports whether reader is the exact reader this cache was filled
// through. Readers of uncomparable types are never treated as the same.
func (s *Cache) ReadsWith(reader Reader) bool {
	if s == nil || reader == nil || s.reader == nil {
		return false
	}
	t := reflect.TypeOf(reader)
	return t == reflect.TypeOf(s.reader) && t.Comparable() && reader == s.reader
}

func (s *Cache) retain(size int64) bool {
	if s.limit > 0 && s.bytes+size > s.limit {
		return false
	}
	s.bytes += size
	return true
}

// Archive reads immutable repository files and in-repository link paths.
func (s *Cache) Archive(ctx context.Context, repository, sha string) (map[string][]byte, []string, error) {
	content, err := fetchcache.Load(ctx, &s.mu, s.archives, repository+"@"+sha, func(ctx context.Context) (archiveContent, error) {
		files, links, err := s.reader.Archive(ctx, repository, sha)
		return archiveContent{files, links}, err
	}, func(a archiveContent) bool { return s.retain(ArchiveSize(a.files)) })
	if err == nil && s.HoldsArchive(repository, sha) {
		s.indexHeldArchive(repository, sha)
	}
	return content.files, content.links, err
}

// Tree lists repository at sha through the cache, so later renders of the
// same revision reuse the listing.
func (s *Cache) Tree(ctx context.Context, repository, sha string) ([]github.TreeEntry, error) {
	reader, ok := s.reader.(TreeReader)
	if !ok {
		return nil, domain.Fail(domain.CodeNotFound, "lookup", "source tree listing is unavailable", "read the full source archive", nil)
	}
	return fetchcache.Load(ctx, &s.mu, s.trees, repository+"@"+sha, func(ctx context.Context) ([]github.TreeEntry, error) {
		return reader.Tree(ctx, repository, sha)
	}, nil)
}

// Blob reads repository content by SHA, reusing a kept archive when possible.
func (s *Cache) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	reader, ok := s.reader.(TreeReader)
	if !ok {
		return nil, domain.Fail(domain.CodeNotFound, "lookup", "source blob reading is unavailable", "read the full source archive", nil)
	}
	if data, ok := s.blobFromArchive(repository, sha); ok {
		return data, nil
	}
	// The key includes the repository: a failure such as access_denied
	// belongs to the repository read, not to content shared with another.
	return fetchcache.Load(ctx, &s.mu, s.blobs, repository+"@"+sha, func(ctx context.Context) ([]byte, error) {
		return reader.Blob(ctx, repository, sha)
	}, func(data []byte) bool { return s.retain(int64(len(data))) })
}

// blobFromArchive returns the content of blob sha of repository from a kept
// archive whose kept tree listing names that blob. A blob SHA identifies its
// content, so a file unchanged between revisions is served from the archive
// of either.
func (s *Cache) blobFromArchive(repository, sha string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	location, ok := s.held[repository][sha]
	if !ok {
		return nil, false
	}
	content, ok := s.archives[repository+"@"+location.revision].Cached()
	if !ok {
		return nil, false
	}
	data, ok := content.files[location.path]
	return data, ok
}

// indexHeldArchive records the blobs of a kept archive by SHA from the
// revision's kept tree listing, for blobFromArchive.
func (s *Cache) indexHeldArchive(repository, revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listing, ok := s.trees[repository+"@"+revision].Cached()
	if !ok {
		return
	}
	if s.held == nil {
		s.held = map[string]map[string]heldBlob{}
	}
	if s.held[repository] == nil {
		s.held[repository] = map[string]heldBlob{}
	}
	for _, entry := range listing {
		if !entry.Link() {
			s.held[repository][entry.SHA] = heldBlob{revision: revision, path: entry.Path}
		}
	}
}

// HoldsArchive reports whether the cache keeps the finished archive of
// repository at sha, without waiting for or starting a read.
func (s *Cache) HoldsArchive(repository, sha string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	f := s.archives[repository+"@"+sha]
	s.mu.Unlock()
	_, ok := f.Cached()
	return ok
}

// ArchiveReader returns a reader of repository files at sha that reads the
// revision's archive once, on its first call, through this cache, so later
// renders of the revision can reuse it instead of reading files one by one.
// entries is the revision's tree listing. It returns nil when the listed
// files exceed limit or the archive entry bound. After a failed archive read
// every call returns that error; callers then read files individually.
func (s *Cache) ArchiveReader(repository, sha string, entries []github.TreeEntry, limit int64) func(context.Context, string) ([]byte, bool, error) {
	var total int64
	for _, entry := range entries {
		total += entry.Size
	}
	if s == nil || len(entries) == 0 || len(entries) > MaxArchiveEntries || limit <= 0 || total > limit {
		return nil
	}
	var mu sync.Mutex
	var files map[string][]byte
	var failed error
	return func(ctx context.Context, file string) ([]byte, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if files == nil && failed == nil {
			content, _, err := s.Archive(ctx, repository, sha)
			if err != nil {
				if ctx.Err() != nil {
					return nil, false, err
				}
				failed = err
			}
			files = content
		}
		if failed != nil {
			return nil, false, failed
		}
		data, ok := files[file]
		return data, ok, nil
	}
}

// ArchiveSize counts the content bytes of repository files.
func ArchiveSize(files map[string][]byte) int64 {
	var size int64
	for _, data := range files {
		size += int64(len(data))
	}
	return size
}
