package gitlocal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/github"
)

// fakeAPI records provider calls and serves fixed content for fallbacks.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}
func (f *fakeAPI) Archive(context.Context, string, string) (map[string][]byte, []string, error) {
	f.record("archive")
	return map[string][]byte{"from-api": []byte("api")}, nil, nil
}
func (f *fakeAPI) Resolve(context.Context, string, string) (string, error) {
	f.record("resolve")
	return strings.Repeat("f", 40), nil
}
func (f *fakeAPI) Tree(context.Context, string, string) ([]github.TreeEntry, error) {
	f.record("tree")
	return nil, nil
}
func (f *fakeAPI) Blob(context.Context, string, string) ([]byte, error) {
	f.record("blob")
	return []byte("api"), nil
}
func (f *fakeAPI) File(context.Context, string, string, string) ([]byte, bool, error) {
	f.record("file")
	return []byte("api"), true, nil
}

// repository creates a local repository with one commit and returns its
// directory and commit SHA.
func repository(t *testing.T, files map[string]string, links map[string]string) (string, string, string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command(gitPath, append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + dir}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	for name, data := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skip("symbolic links are unavailable")
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	return gitPath, dir, git("rev-parse", "HEAD")
}

var fixture = map[string]string{
	"charts/api/Chart.yaml":            "apiVersion: v2\nname: api\nversion: 0.1.0\n",
	"charts/api/values.yaml":           "replicas: 2\n",
	"charts/api/templates/deploy.yaml": "kind: Deployment\n",
}

// Content of a commit the local repository holds is read locally, exactly
// as the provider would return it; references always come from the provider.
func TestLocalRepositoryServesImmutableContent(t *testing.T) {
	gitPath, dir, sha := repository(t, fixture, map[string]string{"charts/api/shared": "templates"})
	api := &fakeAPI{}
	r := &Reader{API: api, Executable: gitPath, Paths: map[string]string{"example/charts": dir}, Limit: 1 << 20}
	ctx := context.Background()

	files, links, err := r.Archive(ctx, "example/charts", sha)
	if err != nil || len(files) != 3 || string(files["charts/api/values.yaml"]) != "replicas: 2\n" || len(links) != 1 || links[0] != "charts/api/shared" {
		t.Fatalf("archive: files=%d links=%v err=%v", len(files), links, err)
	}
	entries, err := r.Tree(ctx, "example/charts", sha)
	if err != nil || len(entries) != 4 {
		t.Fatalf("tree: %v %v", entries, err)
	}
	var chart github.TreeEntry
	for _, entry := range entries {
		if entry.Path == "charts/api/Chart.yaml" {
			chart = entry
		}
	}
	if chart.Type != "blob" || chart.Size != int64(len(fixture["charts/api/Chart.yaml"])) || !github.ValidSHA(chart.SHA) {
		t.Fatalf("tree entry: %+v", chart)
	}
	if data, err := r.Blob(ctx, "example/charts", chart.SHA); err != nil || string(data) != fixture["charts/api/Chart.yaml"] {
		t.Fatalf("blob: %q %v", data, err)
	}
	if data, found, err := r.File(ctx, "example/charts", sha, "charts/api/Chart.yaml"); err != nil || !found || string(data) != fixture["charts/api/Chart.yaml"] {
		t.Fatalf("file: %q %t %v", data, found, err)
	}
	if _, found, err := r.File(ctx, "example/charts", sha, "charts/api/requirements.yaml"); err != nil || found {
		t.Fatalf("an absent file must read as not found: %t %v", found, err)
	}
	for _, notFile := range []string{"charts/api", "charts/api/shared"} {
		var failure *domain.Failure
		if _, _, err := r.File(ctx, "example/charts", sha, notFile); !errors.As(err, &failure) || failure.Issue.Code != domain.CodeResponseInvalid {
			t.Fatalf("%s must not read as a file: %v", notFile, err)
		}
	}
	if len(api.calls) != 0 {
		t.Fatalf("provider called for local content: %v", api.calls)
	}
	if _, err := r.Resolve(ctx, "example/charts", "main"); err != nil || len(api.calls) != 1 || api.calls[0] != "resolve" {
		t.Fatalf("references must come from the provider: %v", api.calls)
	}
	if notes := r.Notes(); len(notes) != 0 {
		t.Fatalf("notes for a complete local run: %v", notes)
	}
}

// A commit, blob or repository the local copy cannot serve is read through
// the provider, and the report says so.
func TestMissingLocalContentFallsBackToTheProvider(t *testing.T) {
	gitPath, dir, _ := repository(t, fixture, nil)
	api := &fakeAPI{}
	broken := t.TempDir()
	r := &Reader{API: api, Executable: gitPath, Paths: map[string]string{"example/charts": dir, "example/broken": broken}, Limit: 1 << 20}
	ctx := context.Background()
	unfetched := strings.Repeat("e", 40)
	if files, _, err := r.Archive(ctx, "example/charts", unfetched); err != nil || string(files["from-api"]) != "api" {
		t.Fatalf("unfetched commit: %v %v", files, err)
	}
	if data, err := r.Blob(ctx, "example/charts", strings.Repeat("d", 40)); err != nil || string(data) != "api" {
		t.Fatalf("missing blob: %q %v", data, err)
	}
	if _, _, err := r.File(ctx, "example/broken", unfetched, "a.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Archive(ctx, "example/other", unfetched); err != nil {
		t.Fatal(err)
	}
	if strings.Join(api.calls, ",") != "archive,blob,file,archive" {
		t.Fatalf("provider calls = %v", api.calls)
	}
	notes := strings.Join(r.Notes(), "\n")
	if !strings.Contains(notes, "Local repository example/broken could not be read") || !strings.Contains(notes, "1 commit(s) were not in the configured local repositories") {
		t.Fatalf("notes = %s", notes)
	}
}

// partialClone clones src without file contents, as a blob-filtered partial
// clone does, and returns its directory.
func partialClone(t *testing.T, gitPath, src string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + t.TempDir()}
	for _, args := range [][]string{
		{"-C", src, "config", "uploadpack.allowFilter", "true"},
		{"clone", "-q", "--filter=blob:none", "--no-checkout", "file://" + filepath.ToSlash(src), dir},
	} {
		command := exec.Command(gitPath, args...)
		command.Env = env
		if out, err := command.CombinedOutput(); err != nil {
			t.Skipf("partial clone unavailable: %v %s", err, out)
		}
	}
	return dir
}

// A commit the local repository holds without its file contents, as in a
// partial clone, is read through the provider like a commit it does not
// hold: a file is never reported absent and an archive never fails as
// truncated because a blob was not fetched.
func TestPartialCloneContentFallsBackToTheProvider(t *testing.T) {
	gitPath, src, sha := repository(t, fixture, nil)
	clone := partialClone(t, gitPath, src)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		read func(r *Reader) error
		call string
	}{
		{"file", func(r *Reader) error {
			data, found, err := r.File(ctx, "example/charts", sha, "charts/api/values.yaml")
			if err == nil && (!found || string(data) != "api") {
				return errors.New("file read as absent or local")
			}
			return err
		}, "file"},
		{"archive", func(r *Reader) error {
			files, _, err := r.Archive(ctx, "example/charts", sha)
			if err == nil && string(files["from-api"]) != "api" {
				return errors.New("archive read locally")
			}
			return err
		}, "archive"},
		{"tree", func(r *Reader) error {
			_, err := r.Tree(ctx, "example/charts", sha)
			return err
		}, "tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			r := &Reader{API: api, Executable: gitPath, Paths: map[string]string{"example/charts": clone}, Limit: 1 << 20}
			if err := tc.read(r); err != nil {
				t.Fatal(err)
			}
			if strings.Join(api.calls, ",") != tc.call {
				t.Fatalf("provider calls = %v, want %s", api.calls, tc.call)
			}
			// Later reads of the commit go to the provider directly.
			if err := tc.read(r); err != nil || strings.Join(api.calls, ",") != tc.call+","+tc.call {
				t.Fatalf("second read: provider calls = %v, %v", api.calls, err)
			}
			if notes := strings.Join(r.Notes(), "\n"); !strings.Contains(notes, "1 commit(s) were in the configured local repositories without all file contents, as in a partial clone") {
				t.Fatalf("notes = %s", notes)
			}
		})
	}
}

// Local archives keep the provider's safety rules and bounds.
func TestLocalArchiveRejectsEscapingLinksAndBounds(t *testing.T) {
	gitPath, dir, sha := repository(t, fixture, map[string]string{"charts/api/escape": "../../../outside"})
	r := &Reader{API: &fakeAPI{}, Executable: gitPath, Paths: map[string]string{"example/charts": dir}, Limit: 1 << 20}
	var failure *domain.Failure
	if _, _, err := r.Archive(context.Background(), "example/charts", sha); !errors.As(err, &failure) || failure.Issue.Code != domain.CodeSourceArchiveFailed {
		t.Fatalf("escaping link accepted: %v", err)
	}
	gitPath, dir, sha = repository(t, fixture, nil)
	small := &Reader{API: &fakeAPI{}, Executable: gitPath, Paths: map[string]string{"example/charts": dir}, Limit: 8}
	if _, _, err := small.Archive(context.Background(), "example/charts", sha); !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLimitExceeded {
		t.Fatalf("archive beyond the byte limit accepted: %v", err)
	}
	// The byte bound names the setting that raises it, like the provider's.
	if !strings.Contains(failure.Issue.Reason, "source archive exceeds render.maxResponseBytes") || !strings.Contains(failure.Issue.Reason, "limit 8 bytes") || !strings.Contains(failure.Issue.Action, "increase render.maxResponseBytes") {
		t.Fatalf("byte bound does not name render.maxResponseBytes: %+v", failure.Issue)
	}
}

// Local repository output is held only up to its bound: a file far over
// render.maxResponseBytes stops the read at the bound instead of being read
// whole and rejected afterwards.
func TestLocalReadStopsAtTheOutputBound(t *testing.T) {
	gitPath, dir, sha := repository(t, map[string]string{"charts/api/large.yaml": strings.Repeat("x", 1<<20)}, nil)
	r := &Reader{API: &fakeAPI{}, Executable: gitPath, Paths: map[string]string{"example/charts": dir}, Limit: 8}
	var failure *domain.Failure
	if _, _, err := r.File(context.Background(), "example/charts", sha, "charts/api/large.yaml"); !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLimitExceeded || failure.Issue.Reason != "local repository output exceeds its byte limit" {
		t.Fatalf("output past the bound was read: %v", err)
	}
}

// The file listing has its own fixed bound, which render.maxResponseBytes
// does not raise, so its failure must not name that setting.
func TestLocalListingBoundNamesItsOwnLimit(t *testing.T) {
	gitPath, dir, sha := repository(t, fixture, nil)
	r := &Reader{API: &fakeAPI{}, Executable: gitPath, Paths: map[string]string{"example/charts": dir}, Limit: 1 << 20}
	bound := maxListingBytes
	t.Cleanup(func() { maxListingBytes = bound })
	maxListingBytes = 16
	var failure *domain.Failure
	if _, err := r.Tree(context.Background(), "example/charts", sha); !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLimitExceeded || !strings.Contains(failure.Issue.Reason, "file listing") || strings.Contains(failure.Issue.Reason+failure.Issue.Action, "render.maxResponseBytes") {
		t.Fatalf("listing bound reported as render.maxResponseBytes: %v", err)
	}
}
