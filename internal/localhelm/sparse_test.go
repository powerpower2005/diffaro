package localhelm

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/github"
)

// sparseGit serves a tree and blobs; the archive must not be needed for large
// repositories.
type sparseGit struct {
	files    map[string]string
	links    map[string]string
	archives int
	mu       sync.Mutex
	blobs    map[string]int
}

func blobSHA(data string) string { sum := sha1.Sum([]byte(data)); return hex.EncodeToString(sum[:]) }

func (g *sparseGit) Archive(context.Context, string, string) (map[string][]byte, []string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.archives++
	out := map[string][]byte{}
	for name, data := range g.files {
		out[name] = []byte(data)
	}
	var links []string
	for name := range g.links {
		links = append(links, name)
	}
	return out, links, nil
}
func (g *sparseGit) Resolve(context.Context, string, string) (string, error) { return testSHA, nil }
func (g *sparseGit) Tree(context.Context, string, string) ([]github.TreeEntry, error) {
	var out []github.TreeEntry
	for name, data := range g.files {
		out = append(out, github.TreeEntry{Path: name, Mode: "100644", Type: "blob", SHA: blobSHA(data), Size: int64(len(data))})
	}
	for name, target := range g.links {
		out = append(out, github.TreeEntry{Path: name, Mode: "120000", Type: "blob", SHA: blobSHA(target), Size: int64(len(target))})
	}
	return out, nil
}
func (g *sparseGit) Blob(_ context.Context, _ string, sha string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blobs == nil {
		g.blobs = map[string]int{}
	}
	g.blobs[sha]++
	for _, data := range g.files {
		if blobSHA(data) == sha {
			return []byte(data), nil
		}
	}
	for _, data := range g.links {
		if blobSHA(data) == sha {
			return []byte(data), nil
		}
	}
	return nil, errors.New("missing blob")
}

func largeRepository() *sparseGit {
	return &sparseGit{files: map[string]string{
		"apps/api/Chart.yaml":           "apiVersion: v2\nname: api\nversion: 0.1.0\ndependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../../libs/common\n",
		"apps/api/templates/cm.yaml":    "kind: ConfigMap",
		"apps/api/values.yaml":          "replicas: 1",
		"env/prod.yaml":                 "replicas: 3",
		"libs/common/Chart.yaml":        "apiVersion: v2\nname: common\nversion: 0.1.0\n",
		"libs/common/templates/_h.tpl":  "{{/* helper */}}",
		"unrelated/huge.bin":            strings.Repeat("x", sparseThreshold+1),
		"unrelated/other/templates.txt": "not needed",
	}, links: map[string]string{".tools/hooks": "../scripts/hooks"}}
}

func TestSparseSourceReadsOnlyRenderInputs(t *testing.T) {
	git := largeRepository()
	c := &Client{Git: git, SnapshotLimit: 64 << 20}
	plan := []domain.SourceRevision{{Position: 1, Repository: testRepo, Revision: testSHA, Source: map[string]any{"path": "apps/api", "helm": map[string]any{"valueFiles": []any{"values.yaml", "../../env/prod.yaml"}}}}}
	needs := planNeeds(Request{Plan: plan})
	files, links, err := c.sourceFiles(context.Background(), "example/charts", testSHA, needs[testRepo+"@"+testSHA])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"apps/api/Chart.yaml", "apps/api/templates/cm.yaml", "apps/api/values.yaml", "env/prod.yaml", "libs/common/Chart.yaml", "libs/common/templates/_h.tpl"} {
		if _, ok := files[name]; !ok {
			t.Errorf("render input %s not read", name)
		}
	}
	if _, ok := files["unrelated/huge.bin"]; ok || len(files) != 6 || git.archives != 0 || len(links) != 1 {
		t.Fatalf("sparse read fetched unrelated files or the archive: files=%d archives=%d links=%v", len(files), git.archives, links)
	}
	// A second revision with identical content reuses cached blobs.
	if _, _, err := c.sourceFiles(context.Background(), "example/charts", strings.Repeat("b", 40), needs[testRepo+"@"+testSHA]); err != nil {
		t.Fatal(err)
	}
	for sha, count := range git.blobs {
		if count != 1 {
			t.Fatalf("blob %s read %d times", sha, count)
		}
	}
}

func TestSparseSourceFallsBackAndRejectsEscapingLinks(t *testing.T) {
	small := &sparseGit{files: map[string]string{"app/Chart.yaml": "name: app"}}
	c := &Client{Git: small}
	if _, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{"app": true}, files: map[string]bool{}}); err != nil || small.archives != 1 {
		t.Fatalf("small repository must use the archive: %v archives=%d", err, small.archives)
	}
	escaping := largeRepository()
	escaping.links["bad"] = "../../outside"
	c = &Client{Git: escaping, SnapshotLimit: 64 << 20}
	var failure *domain.Failure
	if _, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}}); !errors.As(err, &failure) || failure.Issue.Code != "source_archive_failed" {
		t.Fatalf("escaping link accepted: %v", err)
	}
}

// A repository under the small-repository threshold that exceeds
// render.maxResponseBytes is read file by file, as documented, instead of
// failing as an archive over the response bound.
func TestSmallRepositoryOverResponseLimitIsReadFileByFile(t *testing.T) {
	needs := &sourceNeeds{dirs: map[string]bool{"app": true}, files: map[string]bool{}}
	for _, tc := range []struct {
		name     string
		limit    int64
		archives int
	}{
		{"fits the response limit", 1 << 20, 1},
		{"exceeds the response limit", 64 << 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := &sparseGit{files: map[string]string{
				"app/Chart.yaml":           "apiVersion: v2\nname: app\nversion: 0.1.0\n",
				"app/templates/cm.yaml":    "kind: ConfigMap",
				"docs/large-reference.txt": strings.Repeat("x", 128<<10),
			}}
			c := &Client{Git: git, Limit: tc.limit, SnapshotLimit: 64 << 20}
			files, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, needs)
			if err != nil || git.archives != tc.archives || files["app/Chart.yaml"] == nil || files["app/templates/cm.yaml"] == nil {
				t.Fatalf("archives=%d, want %d; err=%v", git.archives, tc.archives, err)
			}
			if tc.archives == 0 && (files["docs/large-reference.txt"] != nil || len(git.blobs) != 2) {
				t.Fatalf("file-by-file read fetched unneeded files: %d files, blobs %v", len(files), git.blobs)
			}
		})
	}
}

func TestPlanNeedsIncludesReferencedValueFiles(t *testing.T) {
	plan := []domain.SourceRevision{
		{Position: 1, Repository: "chart", Revision: "a", Source: map[string]any{"path": "charts/app", "helm": map[string]any{"valueFiles": []any{"values.yaml", "$values/env/prod.yaml", "https://example.invalid/remote.yaml"}, "fileParameters": []any{map[string]any{"path": "files/config.json"}}}}},
		{Position: 2, Repository: "values", Revision: "b", Ref: "values", Source: map[string]any{"ref": "values"}},
	}
	needs := planNeeds(Request{Plan: plan})
	chart, values := needs["chart@a"], needs["values@b"]
	if !chart.dirs["charts/app"] || !chart.files["charts/app/values.yaml"] || !chart.files["charts/app/files/config.json"] || len(chart.files) != 2 || !values.files["env/prod.yaml"] || len(values.dirs) != 0 {
		t.Fatalf("needs: chart=%+v values=%+v", chart, values)
	}
}

func TestSparseSourceReadsRootRelativeAndExpandedValueFiles(t *testing.T) {
	git := largeRepository()
	git.files["env/api.yaml"] = "replicas: 4"
	c := &Client{Git: git, SnapshotLimit: 64 << 20}
	req := fixtureRequest()
	// Argo CD reads "/env/prod.yaml" from the repository root and expands
	// $ARGOCD_APP_NAME; both must be fetched when the repository is read file
	// by file, or ignoreMissingValueFiles would silently drop them.
	req.Plan = []domain.SourceRevision{{Position: 1, Repository: testRepo, Revision: testSHA, Source: map[string]any{"path": "apps/api", "helm": map[string]any{"ignoreMissingValueFiles": true, "valueFiles": []any{"/env/prod.yaml", "../../env/$ARGOCD_APP_NAME.yaml"}}}}}
	files, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, planNeeds(req)[testRepo+"@"+testSHA])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"env/prod.yaml", "env/api.yaml"} {
		if _, ok := files[name]; !ok {
			t.Errorf("value file %s not read by the sparse path", name)
		}
	}
	if git.archives != 0 {
		t.Fatal("the sparse path read the archive")
	}
}

type treeFailingGit struct {
	*sparseGit
	err error
}

func (g treeFailingGit) Tree(context.Context, string, string) ([]github.TreeEntry, error) {
	return nil, g.err
}

func TestSparseSourceFallsBackOnlyWhenTreesAreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{"missing trees API", domain.Fail("not_found", "lookup", "missing", "check", nil), true},
		{"truncated listing", domain.Fail("limit_exceeded", "source", "source tree listing is truncated", "read the full source archive", nil), true},
		{"rate limit", domain.Fail("rate_limited", "lookup", "limited", "retry later", nil), false},
		{"server error", domain.Fail("backend_error", "lookup", "unavailable", "retry", nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := treeFailingGit{largeRepository(), tc.err}
			c := &Client{Git: git, SnapshotLimit: 64 << 20}
			_, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}})
			if tc.fallback && (err != nil || git.archives != 1) {
				t.Fatalf("unavailable trees must fall back to the archive: %v archives=%d", err, git.archives)
			}
			if !tc.fallback && (!errors.Is(err, tc.err) || git.archives != 0) {
				t.Fatalf("a transient tree failure must be reported as it is, not as an archive result: %v archives=%d", err, git.archives)
			}
		})
	}
}

// Many needed files read one archive when the repository fits the limits;
// otherwise they are read file by file.
func TestSparseSourceUsesArchiveForManyFiles(t *testing.T) {
	needs := &sourceNeeds{dirs: map[string]bool{"apps/big": true}, files: map[string]bool{}}
	for _, tc := range []struct {
		name     string
		limit    int64
		archives int
	}{
		{"fits", 64 << 20, 1},
		{"exceeds response limit", sparseThreshold, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := largeRepository()
			for i := 0; i <= maxSparseBlobs; i++ {
				git.files["apps/big/templates/"+strings.Repeat("x", i+1)+".yaml"] = "kind: ConfigMap\n# " + strings.Repeat("y", i)
			}
			c := &Client{Git: git, Limit: tc.limit, SnapshotLimit: 64 << 20}
			files, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, needs)
			if err != nil || git.archives != tc.archives {
				t.Fatalf("archives=%d err=%v", git.archives, err)
			}
			if len(files) < maxSparseBlobs+1 {
				t.Fatalf("render inputs missing: %d", len(files))
			}
			if tc.archives == 1 {
				if len(git.blobs) != 0 {
					t.Fatalf("archive path also read blobs: %v", git.blobs)
				}
				// Later renders of the same revision reuse the archive.
				if _, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}}); err != nil || git.archives != 1 || len(git.blobs) != 0 {
					t.Fatalf("cached archive not reused: archives=%d blobs=%v err=%v", git.archives, git.blobs, err)
				}
			}
		})
	}
}

// Renders of one revision share the file-by-file budget, so many small charts
// also switch to one archive.
func TestSparseSourceBudgetIsPerRevision(t *testing.T) {
	git := largeRepository()
	for chart := 0; chart < 3; chart++ {
		for i := 0; i < maxSparseBlobs/2; i++ {
			name := "apps/c" + strings.Repeat("x", chart+1) + "/templates/" + strings.Repeat("f", i+1) + ".yaml"
			git.files[name] = "kind: ConfigMap\n# " + name
		}
	}
	c := &Client{Git: git, Limit: 64 << 20, SnapshotLimit: 64 << 20}
	for chart := 0; chart < 3; chart++ {
		dir := "apps/c" + strings.Repeat("x", chart+1)
		if _, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{dir: true}, files: map[string]bool{}}); err != nil {
			t.Fatal(err)
		}
	}
	// The budget plus the link check bounds file reads.
	if git.archives != 1 || len(git.blobs) > maxSparseBlobs+1 {
		t.Fatalf("archives=%d blob reads=%d", git.archives, len(git.blobs))
	}
}

// Two sources on one repository revision read it once: the second source
// reuses the prepared files instead of charging the file-by-file budget again,
// which would switch the revision to a full archive read.
func TestReadSourcesReadsOneRevisionOnceForTwoSources(t *testing.T) {
	git := largeRepository()
	var plan []domain.SourceRevision
	for i, chart := range []string{"a", "b"} {
		git.files["apps/"+chart+"/Chart.yaml"] = "apiVersion: v2\nname: " + chart + "\nversion: 0.1.0\n"
		for f := 0; f < 19; f++ {
			name := "apps/" + chart + "/templates/" + strings.Repeat("f", f+1) + ".yaml"
			git.files[name] = "kind: ConfigMap\n# " + name
		}
		plan = append(plan, domain.SourceRevision{Position: i + 1, Repository: testRepo, Revision: testSHA, Source: map[string]any{"path": "apps/" + chart, "helm": map[string]any{}}})
	}
	c := &Client{Git: git, RepositoryURL: testRepo, Limit: 64 << 20, SnapshotLimit: 64 << 20}
	request := Request{Plan: plan, Repositories: map[string]map[string][]byte{}, Limit: 1 << 20}
	if err := c.readSources(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	files := request.Repositories[testRepo+"@"+testSHA]
	for _, name := range []string{"apps/a/Chart.yaml", "apps/b/Chart.yaml", "apps/b/templates/f.yaml"} {
		if files[name] == nil {
			t.Fatalf("render input %s not read", name)
		}
	}
	blobs := 0
	for _, count := range git.blobs {
		blobs += count
	}
	// 40 chart files and one link target, each read once; no archive.
	if git.archives != 0 || blobs != 41 || len(git.blobs) != 41 {
		t.Fatalf("archives=%d blob reads=%d distinct=%d", git.archives, blobs, len(git.blobs))
	}
}
