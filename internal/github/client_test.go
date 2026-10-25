package github

import (
	"context"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForkIsRejectedBeforeComparison(t *testing.T) {
	compareCalled := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/compare/") {
			compareCalled = true
		}
		fmt.Fprint(w, `{"base":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"full_name":"example/charts"}},"head":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"full_name":"outside/charts"}}}`)
	}))
	defer s.Close()
	c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1024)}
	if _, err := c.PullRequest(context.Background(), "example/charts", 1); err == nil {
		t.Fatal("untrusted fork accepted")
	}
	if compareCalled {
		t.Fatal("fork was processed before trust validation")
	}
}

func TestImmutableResolution(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"sha":"main"}`) }))
	defer s.Close()
	c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1024)}
	if _, err := c.Resolve(context.Background(), "example/charts", "main"); err == nil {
		t.Fatal("mutable ref passed as SHA")
	}
	for _, ref := range []string{"../token", "/main", "branch\nheader"} {
		if _, err := c.Resolve(context.Background(), "example/charts", ref); err == nil {
			t.Fatal("unsafe ref accepted")
		}
	}
}

func TestUnresolvableRefIsMissingRevision(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusUnprocessableEntity, "not_found"},
		{http.StatusInternalServerError, "backend_error"},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, `{"message":"No commit found for SHA: deleted-branch"}`)
		}))
		c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1024)}
		_, err := c.Resolve(context.Background(), "example/charts", "deleted-branch")
		s.Close()
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != tc.code || failure.Issue.Repository != "example/charts" {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		if tc.status == http.StatusUnprocessableEntity && (failure.Issue.Stage != "revision" || failure.Issue.Revision != "deleted-branch" || !strings.Contains(failure.Issue.Reason, `"deleted-branch" names no commit`) || strings.Contains(failure.Issue.Action, "gateway")) {
			t.Fatalf("missing ref reported as backend fault: %+v", failure.Issue)
		}
	}
}

func TestChangedFilesPaginatesAndDetectsTruncation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		total    int
		listed   int
		complete bool
	}{
		{"two pages", 150, 150, true},
		{"exact page", 100, 100, true},
		{"count mismatch", 151, 150, false},
		{"GitHub cap", 3000, 3000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example/charts/pulls/7" {
					fmt.Fprintf(w, `{"changed_files":%d}`, tc.total)
					return
				}
				if r.URL.Path != "/repos/example/charts/pulls/7/files" || r.URL.Query().Get("per_page") != "100" {
					t.Errorf("unexpected request %s", r.URL)
				}
				pages++
				start := (pages - 1) * 100
				items := []string{}
				for i := start; i < tc.listed && i < start+100; i++ {
					if i == 0 {
						items = append(items, `{"filename":"charts/new/a.yaml","previous_filename":"charts/old/a.yaml","status":"renamed"}`)
						continue
					}
					status := [...]string{"modified", "added", "removed"}[i%3]
					items = append(items, fmt.Sprintf(`{"filename":"charts/x/%d.yaml","status":%q}`, i, status))
				}
				fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
			}))
			defer s.Close()
			c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
			files, states, complete, err := c.ChangedFiles(context.Background(), "example/charts", 7)
			if err != nil || complete != tc.complete || len(files) != tc.listed+1 || files[0] != "charts/new/a.yaml" || files[1] != "charts/old/a.yaml" {
				t.Fatalf("files=%d complete=%t err=%v", len(files), complete, err)
			}
			// A rename adds its new path and removes the previous one; a
			// modified path has no entry and stays changed.
			if states["charts/new/a.yaml"] != domain.StateAdded || states["charts/old/a.yaml"] != domain.StateRemoved {
				t.Fatalf("rename states = %v", states)
			}
			if tc.listed > 3 {
				if _, ok := states["charts/x/3.yaml"]; ok || states["charts/x/1.yaml"] != domain.StateAdded || states["charts/x/2.yaml"] != domain.StateRemoved {
					t.Fatalf("file states = %v %v %v", states["charts/x/1.yaml"], states["charts/x/2.yaml"], states["charts/x/3.yaml"])
				}
			}
			if pages > 30 {
				t.Fatal("listing exceeded GitHub's page bound")
			}
		})
	}
}

func TestFileReadsImmutableRevisionAndReportsAbsence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			t.Errorf("mutable ref %q", r.URL.Query().Get("ref"))
		}
		switch r.URL.Path {
		case "/repos/example/charts/contents/charts/api/Chart.yaml":
			fmt.Fprint(w, `{"type":"file","encoding":"base64","content":"YXBpVmVyc2lvbjog\ndjIK"}`)
		case "/repos/example/charts/contents/charts/api":
			fmt.Fprint(w, `[{"type":"file"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	data, found, err := c.File(context.Background(), "example/charts", sha, "charts/api/Chart.yaml")
	if err != nil || !found || string(data) != "apiVersion: v2\n" {
		t.Fatalf("data=%q found=%t err=%v", data, found, err)
	}
	if _, found, err = c.File(context.Background(), "example/charts", sha, "charts/api/requirements.yaml"); err != nil || found {
		t.Fatal("missing file was not reported as absent", err)
	}
	if _, _, err = c.File(context.Background(), "example/charts", sha, "charts/api"); err == nil {
		t.Fatal("directory accepted as a file")
	}
	for _, file := range []string{"../token", "/etc/passwd", "charts/../../x", ""} {
		if _, _, err = c.File(context.Background(), "example/charts", sha, file); err == nil {
			t.Fatalf("unsafe path %q accepted", file)
		}
	}
	if _, _, err = c.File(context.Background(), "example/charts", "main", "charts/api/Chart.yaml"); err == nil {
		t.Fatal("mutable revision accepted")
	}
}

func TestChangedFilesRequiresTheResolvedHead(t *testing.T) {
	const resolved, pushed = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "dddddddddddddddddddddddddddddddddddddddd"
	for _, tc := range []struct {
		name     string
		later    string
		complete bool
	}{
		{"head unchanged", resolved, true},
		// A push with the same file count while the files are listed must not
		// let changed-path selection prove Applications unaffected.
		{"head moved during listing", pushed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pulls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/repos/example/charts/pulls/7":
					head := resolved
					if pulls++; pulls > 1 {
						head = tc.later
					}
					fmt.Fprintf(w, `{"changed_files":1,"base":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, head)
				case strings.HasPrefix(r.URL.Path, "/repos/example/charts/compare/"):
					fmt.Fprint(w, `{"merge_base_commit":{"sha":"cccccccccccccccccccccccccccccccccccccccc"}}`)
				case r.URL.Path == "/repos/example/charts/pulls/7/files":
					fmt.Fprint(w, `[{"filename":"charts/api/values.yaml"}]`)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			}))
			defer s.Close()
			c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
			pair, err := c.PullRequest(context.Background(), "example/charts", 7)
			if err != nil || pair.Head != resolved {
				t.Fatalf("pair=%+v err=%v", pair, err)
			}
			files, _, complete, err := c.ChangedFiles(context.Background(), "example/charts", 7)
			if err != nil || complete != tc.complete || len(files) != 1 {
				t.Fatalf("files=%v complete=%t err=%v", files, complete, err)
			}
			if pulls != 2 {
				t.Fatalf("pull request fetched %d times, want once to resolve and once to verify the listing", pulls)
			}
		})
	}
}

func TestArchiveRateLimitIsNotAccessDenied(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s.Close()
	c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	_, _, err := c.Archive(context.Background(), "example/charts", strings.Repeat("a", 40))
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "rate_limited" || failure.Issue.Repository != "example/charts" || !strings.Contains(failure.Issue.Reason, "tarball") {
		t.Fatalf("archive rate limit: %v", err)
	}
}
