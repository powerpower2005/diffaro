package gitsource

import (
	"context"
	"testing"

	"diffaro/internal/github"
)

type memoryReader struct {
	archives, blobs int
	files           map[string][]byte
}

func (r *memoryReader) Resolve(context.Context, string, string) (string, error) { return "commit", nil }
func (r *memoryReader) Archive(context.Context, string, string) (map[string][]byte, []string, error) {
	r.archives++
	return r.files, []string{"link"}, nil
}
func (r *memoryReader) Tree(context.Context, string, string) ([]github.TreeEntry, error) {
	return []github.TreeEntry{{Path: "values.yaml", SHA: "blob", Size: 3, Mode: "100644"}}, nil
}
func (r *memoryReader) Blob(context.Context, string, string) ([]byte, error) {
	r.blobs++
	return []byte("api"), nil
}

func TestCacheRetainsWithinBudgetAndBindsItsReader(t *testing.T) {
	ctx := context.Background()
	for _, limit := range []int64{2, 3} {
		reader := &memoryReader{files: map[string][]byte{"values.yaml": []byte("api")}}
		cache := New(reader, limit)
		if !cache.ReadsWith(reader) || cache.ReadsWith(&memoryReader{}) {
			t.Fatal("cache crossed reader identity")
		}
		for range 2 {
			files, links, err := cache.Archive(ctx, "example/charts", "commit")
			if err != nil || string(files["values.yaml"]) != "api" || len(links) != 1 || links[0] != "link" {
				t.Fatalf("archive content changed at limit %d: %v %v %v", limit, files, links, err)
			}
		}
		wantReads := 1
		if limit == 2 {
			wantReads = 2
		}
		if reader.archives != wantReads || cache.HoldsArchive("example/charts", "commit") != (limit == 3) {
			t.Fatalf("retention budget %d: reads=%d", limit, reader.archives)
		}
	}
}

func TestArchiveContentServesBlobsOnlyWithinRepository(t *testing.T) {
	ctx := context.Background()
	reader := &memoryReader{files: map[string][]byte{"values.yaml": []byte("api")}}
	cache := New(reader, 64)
	if _, err := cache.Tree(ctx, "example/charts", "base"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Archive(ctx, "example/charts", "base"); err != nil {
		t.Fatal(err)
	}
	if data, err := cache.Blob(ctx, "example/charts", "blob"); err != nil || string(data) != "api" || reader.blobs != 0 {
		t.Fatalf("unchanged blob was not served from the archive: %q %v reads=%d", data, err, reader.blobs)
	}
	if _, err := cache.Blob(ctx, "example/other", "blob"); err != nil || reader.blobs != 1 {
		t.Fatalf("content crossed the repository boundary: %v reads=%d", err, reader.blobs)
	}
}
