package localhelm

import (
	"context"
	"errors"
	"sync"
	"testing"

	"diffaro/internal/gitsource"
)

// The archive reader serves many files with one archive read, and a render
// of the same revision then reuses that archive without reading files one by
// one, so changed-path selection and rendering cost one request together.
func TestArchiveReaderServesFilesAndRendersFromOneArchive(t *testing.T) {
	git := largeRepository()
	cache := gitsource.New(git, 64<<20)
	entries, err := cache.Tree(context.Background(), "example/charts", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	read := cache.ArchiveReader("example/charts", testSHA, entries, 64<<20)
	if read == nil {
		t.Fatal("a revision within the limit got no archive reader")
	}
	var wg sync.WaitGroup
	for _, name := range []string{"apps/api/Chart.yaml", "libs/common/Chart.yaml", "env/prod.yaml"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if data, found, err := read(context.Background(), name); err != nil || !found || len(data) == 0 {
				t.Errorf("%s: found=%t err=%v", name, found, err)
			}
		}()
	}
	wg.Wait()
	if _, found, err := read(context.Background(), "apps/api/requirements.yaml"); err != nil || found {
		t.Fatalf("an absent file must read as not found: %t %v", found, err)
	}
	if git.archives != 1 || len(git.blobs) != 0 {
		t.Fatalf("archives=%d blobs=%v, want one archive and no file reads", git.archives, git.blobs)
	}

	c := &Client{Git: git, Sources: cache, SnapshotLimit: 64 << 20}
	needs := &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{"env/prod.yaml": true}}
	files, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, needs)
	if err != nil || len(files["apps/api/Chart.yaml"]) == 0 {
		t.Fatalf("render from the held archive: %v", err)
	}
	if git.archives != 1 || len(git.blobs) != 0 {
		t.Fatalf("render read again: archives=%d blobs=%v", git.archives, git.blobs)
	}

	// An instance whose snapshot budget cannot hold the archive still reads
	// only its render inputs.
	small := &Client{Git: git, Sources: cache, SnapshotLimit: 1 << 10}
	if _, _, err := small.sourceFiles(context.Background(), "example/charts", testSHA, needs); err != nil || len(git.blobs) == 0 {
		t.Fatalf("a held archive beyond the budget must not replace file reads: %v blobs=%v", err, git.blobs)
	}
}

// Without room for the whole revision there is no archive reader, and a
// failed archive read is not repeated.
func TestArchiveReaderLimitsAndFailure(t *testing.T) {
	git := largeRepository()
	cache := gitsource.New(git, 0)
	entries, err := cache.Tree(context.Background(), "example/charts", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if cache.ArchiveReader("example/charts", testSHA, entries, sparseThreshold) != nil {
		t.Fatal("a revision beyond the limit got an archive reader")
	}
	failing := &failingArchive{sparseGit: largeRepository()}
	failingCache := gitsource.New(failing, 0)
	read := failingCache.ArchiveReader("example/charts", testSHA, entries, 64<<20)
	for range 3 {
		if _, _, err := read(context.Background(), "apps/api/Chart.yaml"); err == nil {
			t.Fatal("a failed archive read returned content")
		}
	}
	if failing.calls != 1 {
		t.Fatalf("archive read %d times after failing", failing.calls)
	}
}

type failingArchive struct {
	*sparseGit
	calls int
}

func (g *failingArchive) Archive(context.Context, string, string) (map[string][]byte, []string, error) {
	g.calls++
	return nil, nil, errors.New("fictional archive failure")
}

// A file whose content is unchanged in another revision is served from the
// kept archive of one revision by its blob SHA, so a base render after a
// head archive read does not read those files again.
func TestUnchangedBlobsOfAnotherRevisionComeFromTheHeldArchive(t *testing.T) {
	git := largeRepository()
	cache := gitsource.New(git, 64<<20)
	head, base := testSHA, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	entries, err := cache.Tree(context.Background(), "example/charts", head)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.ArchiveReader("example/charts", head, entries, 64<<20)(context.Background(), "apps/api/Chart.yaml"); err != nil {
		t.Fatal(err)
	}
	c := &Client{Git: git, Sources: cache, SnapshotLimit: 1 << 10}
	needs := &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}}
	files, _, err := c.sourceFiles(context.Background(), "example/charts", base, needs)
	if err != nil || len(files["apps/api/Chart.yaml"]) == 0 {
		t.Fatalf("base render: %v", err)
	}
	// Only the link target is read: links are checked from their own blob,
	// as before; every regular file comes from the head archive.
	link := blobSHA(largeRepository().links[".tools/hooks"])
	if len(git.blobs) != 1 || git.blobs[link] != 1 || git.archives != 1 {
		t.Fatalf("unchanged files were read again: blobs=%v archives=%d", git.blobs, git.archives)
	}
	// Another repository never reads this repository's archive.
	chartBlob := blobSHA(largeRepository().files["apps/api/Chart.yaml"])
	if _, err := cache.Blob(context.Background(), "example/other", chartBlob); err != nil || git.blobs[chartBlob] != 1 {
		t.Fatal("another repository must read through its provider", err, git.blobs)
	}
}
