// Package gitlocal reads repository content addressed by immutable commit or
// blob SHAs from local Git repositories that an operator keeps fetched, such
// as a CI cache updated with git fetch, and delegates mutable references and
// anything not held locally to the Git provider API. It runs only
// read-only git plumbing commands with no user configuration.
package gitlocal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/github"
	"diffaro/internal/procio"
)

// API is the provider reader that serves everything a local repository does
// not: mutable references, content of repositories without a local copy, and
// commits a local copy has not fetched yet.
type API interface {
	Archive(ctx context.Context, repository, revision string) (map[string][]byte, []string, error)
	Resolve(ctx context.Context, repository, ref string) (string, error)
	Tree(ctx context.Context, repository, revision string) ([]github.TreeEntry, error)
	Blob(ctx context.Context, repository, sha string) ([]byte, error)
	File(ctx context.Context, repository, revision, file string) ([]byte, bool, error)
}

// Reader reads repository content addressed by an immutable commit or blob
// SHA from local Git repositories and delegates the rest to API.
// Content is identical to the provider's because Git objects are addressed by
// their SHA. It runs only read-only plumbing commands, never checks out files
// or runs hooks, and never fetches; the operator keeps the repositories
// fetched.
type Reader struct {
	API API
	// Executable is the absolute path of git.
	Executable string
	// Paths maps owner/repository to a local repository directory.
	Paths map[string]string
	// Limit bounds the bytes of one archive, as for provider archives.
	Limit int64

	mu      sync.Mutex
	commits map[string]bool
	missed  map[string]bool
	// partial holds commits the repository holds without all of their file
	// contents, as in a partial clone.
	partial map[string]bool
	failed  map[string]string
}

// errMissingContent reports that a held commit lacks file contents locally.
var errMissingContent = errors.New("local repository lacks file contents of the commit")

// errOutputLimit reports that a git command printed more than its bound;
// each caller names the bound it passed.
var errOutputLimit = errors.New("local repository output exceeds its byte limit")

// maxListingBytes bounds the file listing of one revision. It is fixed, not
// render.maxResponseBytes, and a variable only so tests can lower it.
var maxListingBytes int64 = 64 << 20

// lacking records that the local repository holds revision without all of
// its file contents: this and later reads of it go to the provider, as for a
// commit it does not hold.
func (r *Reader) lacking(repository, revision string) {
	key := repository + "@" + revision
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.partial == nil {
		r.partial = map[string]bool{}
	}
	r.commits[key] = false
	r.partial[key] = true
}

// Resolve always asks the provider: a reference can move, and a local
// repository may not have fetched it yet.
func (r *Reader) Resolve(ctx context.Context, repository, ref string) (string, error) {
	return r.API.Resolve(ctx, repository, ref)
}

func (r *Reader) Archive(ctx context.Context, repository, revision string) (map[string][]byte, []string, error) {
	dir, ok := r.commit(ctx, repository, revision)
	if !ok {
		return r.API.Archive(ctx, repository, revision)
	}
	end := domain.TraceStart(ctx, "source", "local-archive", repository+"@"+revision)
	files, links, err := r.archive(ctx, dir, revision)
	if errors.Is(err, errMissingContent) {
		end(nil)
		r.lacking(repository, revision)
		return r.API.Archive(ctx, repository, revision)
	}
	end(err)
	return files, links, err
}

func (r *Reader) Tree(ctx context.Context, repository, revision string) ([]github.TreeEntry, error) {
	dir, ok := r.commit(ctx, repository, revision)
	if !ok {
		return r.API.Tree(ctx, repository, revision)
	}
	end := domain.TraceStart(ctx, "source", "local-tree", repository+"@"+revision)
	entries, err := r.tree(ctx, dir, revision, "")
	if errors.Is(err, errMissingContent) {
		// Without the contents the listing has no file sizes.
		end(nil)
		r.lacking(repository, revision)
		return r.API.Tree(ctx, repository, revision)
	}
	end(err)
	return blobs(entries), err
}

func (r *Reader) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	dir, ok := r.Paths[repository]
	if !ok || !github.ValidSHA(sha) {
		return r.API.Blob(ctx, repository, sha)
	}
	end := domain.TraceStart(ctx, "source", "local-blob", repository)
	contents, err := r.objects(ctx, dir, []string{sha})
	if err == nil && contents[sha] == nil {
		end(nil)
		return r.API.Blob(ctx, repository, sha)
	}
	end(err)
	if err != nil {
		return nil, err
	}
	return contents[sha], nil
}

func (r *Reader) File(ctx context.Context, repository, revision, file string) ([]byte, bool, error) {
	dir, ok := r.commit(ctx, repository, revision)
	if !ok || !domain.SafeRelativePathAllowingColon(file) {
		return r.API.File(ctx, repository, revision, file)
	}
	end := domain.TraceStart(ctx, "source", "local-file", repository+"@"+revision)
	entries, err := r.tree(ctx, dir, revision, file)
	if errors.Is(err, errMissingContent) {
		end(nil)
		r.lacking(repository, revision)
		return r.API.File(ctx, repository, revision, file)
	}
	if err != nil {
		end(err)
		return nil, false, err
	}
	for _, entry := range entries {
		if entry.Path != file {
			continue
		}
		if entry.Type != "blob" || entry.Link() {
			err = domain.Fail(domain.CodeResponseInvalid, "source", "repository path is not a readable file", "check the repository path", nil)
			end(err)
			return nil, false, err
		}
		contents, err := r.objects(ctx, dir, []string{entry.SHA})
		end(err)
		if err != nil {
			return nil, false, err
		}
		if contents[entry.SHA] == nil {
			// The file exists at revision; only its content is not local.
			r.lacking(repository, revision)
			return r.API.File(ctx, repository, revision, file)
		}
		return contents[entry.SHA], true, nil
	}
	if len(entries) > 0 {
		// The path is a directory, which the provider does not read as a file.
		err = domain.Fail(domain.CodeResponseInvalid, "source", "repository path is not a readable file", "check the repository path", nil)
		end(err)
		return nil, false, err
	}
	end(nil)
	return nil, false, nil
}

// Notes states, for the report, local repositories that could not serve a
// read; those reads went to the provider.
func (r *Reader) Notes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var notes []string
	repositories := make([]string, 0, len(r.failed))
	for repository := range r.failed {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	for _, repository := range repositories {
		notes = append(notes, fmt.Sprintf("Local repository %s could not be read (%s); its sources were read through the Git provider API.", repository, r.failed[repository]))
	}
	missed := 0
	for range r.missed {
		missed++
	}
	if missed > 0 {
		notes = append(notes, fmt.Sprintf("%d commit(s) were not in the configured local repositories and were read through the Git provider API; fetch the local repositories before the run.", missed))
	}
	if len(r.partial) > 0 {
		notes = append(notes, fmt.Sprintf("%d commit(s) were in the configured local repositories without all file contents, as in a partial clone, and were read through the Git provider API; use a full clone.", len(r.partial)))
	}
	return notes
}

// commit returns the local directory of repository when it holds revision.
func (r *Reader) commit(ctx context.Context, repository, revision string) (string, bool) {
	dir, ok := r.Paths[repository]
	if !ok || !github.ValidSHA(strings.ToLower(revision)) {
		return "", false
	}
	key := repository + "@" + revision
	r.mu.Lock()
	known, seen := r.commits[key]
	_, broken := r.failed[repository]
	r.mu.Unlock()
	if broken {
		return "", false
	}
	if seen {
		return dir, known
	}
	_, err := r.run(ctx, dir, nil, 0, "cat-file", "-e", revision+"^{commit}")
	if ctx.Err() != nil {
		return "", false
	}
	// A repository that is not readable at all is reported once; a missing
	// commit is only counted.
	unreadable := false
	if err != nil {
		_, probe := r.run(ctx, dir, nil, 0, "rev-parse", "--git-dir")
		unreadable = probe != nil && ctx.Err() == nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commits == nil {
		r.commits, r.missed, r.failed = map[string]bool{}, map[string]bool{}, map[string]string{}
	}
	switch {
	case unreadable:
		r.failed[repository] = "not a readable Git repository"
	case err != nil:
		r.missed[key] = true
	}
	r.commits[key] = err == nil
	return dir, err == nil
}

// tree lists the regular files, links and submodules of revision, or only
// file when it is set, with the same path checks as provider listings.
func (r *Reader) tree(ctx context.Context, dir, revision, file string) ([]github.TreeEntry, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", "--full-tree", revision}
	if file != "" {
		args = append(args, "--", file)
	}
	out, err := r.run(ctx, dir, nil, maxListingBytes, args...)
	if errors.Is(err, errOutputLimit) {
		return nil, domain.Fail(domain.CodeLimitExceeded, "source", fmt.Sprintf("local repository file listing exceeds its fixed %d MiB limit", maxListingBytes>>20), "remove the repository from git.localRepositories so the Git provider serves it", nil)
	}
	if err != nil {
		return nil, err
	}
	var entries []github.TreeEntry
	missing := false
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 4 {
			return nil, unsafeSource("local repository listing is invalid")
		}
		p := string(name)
		if !domain.SafeRelativePath(p) || !github.ValidSHA(fields[2]) {
			return nil, unsafeSource("source tree contains an unsafe path")
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil && fields[1] == "blob" {
			// git prints a size it cannot read, such as "BAD" for a blob a
			// partial clone has not fetched, instead of a number.
			missing = true
		}
		entries = append(entries, github.TreeEntry{Path: p, Mode: fields[0], Type: fields[1], SHA: fields[2], Size: size})
	}
	if missing {
		return nil, errMissingContent
	}
	return entries, nil
}

// blobs keeps the file entries a provider tree listing returns.
func blobs(entries []github.TreeEntry) []github.TreeEntry {
	out := make([]github.TreeEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == "blob" {
			out = append(out, entry)
		}
	}
	return out
}

// archive reads revision's files like a provider archive: regular files by
// path, links that stay inside the repository by path only, submodules
// skipped, and the provider's file and byte bounds.
func (r *Reader) archive(ctx context.Context, dir, revision string) (map[string][]byte, []string, error) {
	entries, err := r.tree(ctx, dir, revision, "")
	if err != nil {
		return nil, nil, err
	}
	var want []string
	var total int64
	var files, linkEntries []github.TreeEntry
	for _, entry := range entries {
		switch {
		case entry.Type == "commit":
			continue
		case entry.Type != "blob":
			return nil, nil, unsafeSource("source archive contains an unsupported entry")
		case entry.Link():
			linkEntries = append(linkEntries, entry)
		case entry.Mode == "100644" || entry.Mode == "100755":
			files = append(files, entry)
			total += entry.Size
		default:
			return nil, nil, unsafeSource("source archive links or unsafe paths are unsupported")
		}
		want = append(want, entry.SHA)
	}
	if len(files)+len(linkEntries) > github.MaxArchiveEntries {
		return nil, nil, github.ArchiveEntriesError()
	}
	if r.Limit > 0 && total > r.Limit {
		return nil, nil, github.ArchiveBytesError(total, r.Limit, false)
	}
	contents, err := r.objects(ctx, dir, want)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string][]byte, len(files))
	for _, entry := range files {
		data := contents[entry.SHA]
		if data == nil {
			return nil, nil, errMissingContent
		}
		out[entry.Path] = data
	}
	links := []string{}
	for _, entry := range linkEntries {
		if contents[entry.SHA] == nil {
			return nil, nil, errMissingContent
		}
		if !github.InsideLink(entry.Path, string(contents[entry.SHA])) {
			return nil, nil, unsafeSource("source archive links or unsafe paths are unsupported")
		}
		links = append(links, entry.Path)
	}
	return out, links, nil
}

// objects reads blobs by SHA with one cat-file process. A blob the
// repository lacks is absent from the result.
func (r *Reader) objects(ctx context.Context, dir string, shas []string) (map[string][]byte, error) {
	var input bytes.Buffer
	for _, sha := range shas {
		input.WriteString(sha + "\n")
	}
	limit := r.Limit
	if limit <= 0 {
		limit = 512 << 20
	}
	out, err := r.run(ctx, dir, &input, limit+int64(len(shas))*128, "cat-file", "--batch")
	if errors.Is(err, errOutputLimit) {
		return nil, domain.Fail(domain.CodeLimitExceeded, "source", "local repository output exceeds its byte limit", "reduce the source size or raise render.maxResponseBytes", nil)
	}
	if err != nil {
		return nil, err
	}
	contents := make(map[string][]byte, len(shas))
	reader := bufio.NewReader(bytes.NewReader(out))
	for range shas {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, unsafeSource("local repository object stream is invalid")
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 {
			return nil, unsafeSource("local repository object stream is invalid")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > limit {
			return nil, unsafeSource("local repository object stream is invalid")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, unsafeSource("local repository object stream is invalid")
		}
		if b, err := reader.ReadByte(); err != nil || b != '\n' {
			return nil, unsafeSource("local repository object stream is invalid")
		}
		if fields[1] == "blob" {
			contents[fields[0]] = data
		}
	}
	return contents, nil
}

// run executes a read-only git plumbing command in dir with no user or
// system configuration, no prompts, no lazy fetch of missing objects and no
// optional locks. limit bounds stdout; zero discards it.
func (r *Reader) run(ctx context.Context, dir string, stdin io.Reader, limit int64, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir, "-c", "safe.directory=*", "-c", "core.fsmonitor=false", "--no-replace-objects"}, args...)
	command := exec.CommandContext(ctx, r.Executable, full...)
	command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"}
	for _, key := range []string{"SystemRoot", "TMP", "TEMP"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdin = stdin
	command.WaitDelay = time.Second
	output := &procio.Bounded{Limit: limit}
	command.Stdout, command.Stderr = output, io.Discard
	if limit <= 0 {
		command.Stdout = io.Discard
	}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if output.Exceeded {
			return nil, errOutputLimit
		}
		return nil, domain.Fail(domain.CodeSourceFailed, "source", "local Git repository read failed", "check git.localRepositories and git.gitExecutable, and fetch the repository", err)
	}
	return output.Bytes(), nil
}

func unsafeSource(reason string) error {
	return domain.Fail(domain.CodeSourceArchiveFailed, "source", reason, "check repository access, archive limits and supported regular files", nil)
}
