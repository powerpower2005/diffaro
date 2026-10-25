package github_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/github"
	"diffaro/internal/httpx"
	"diffaro/internal/preview"
)

var archiveSHA = strings.Repeat("a", 40)

func archiveIssue(t *testing.T, err error) domain.Issue {
	t.Helper()
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("want a domain failure, got %v", err)
	}
	return failure.Issue
}

// originOf labels a head-only failure built from err the way preview does.
func originOf(err error) domain.FailureOrigin {
	id := domain.ApplicationID{Instance: "fictional", Namespace: "argocd", Name: "app"}
	report := domain.Report{Applications: []domain.ApplicationResult{{
		ID: id, BaseTarget: &domain.TargetSummary{Execution: "complete"}, HeadTarget: &domain.TargetSummary{Execution: "failed"},
		Issues: []domain.Issue{preview.ContextualIssue(err, id, "example/charts", archiveSHA)},
	}}}
	preview.AnnotateFailureOrigins(&report)
	return report.Applications[0].FailureOrigin
}

// An archive rejected by status fails like every other GitHub read of the
// same repository: same code, reason code, stage, access requirement and
// access diagnostics, and a server error or quota rejection is transient.
func TestArchiveStatusFailuresMatchOtherGitHubReads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		header    map[string]string
		code      string
		reason    string
		transient bool
	}{
		{"forbidden", http.StatusForbidden, nil, "access_denied", "request_forbidden", false},
		{"unauthorized", http.StatusUnauthorized, nil, "access_denied", "authentication_rejected", false},
		{"not found", http.StatusNotFound, nil, "not_found", "", false},
		{"server error", http.StatusBadGateway, nil, "backend_error", "server_error", true},
		{"rate limited", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, "rate_limited", "", true},
		{"quota exhausted", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "rate_limited", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer s.Close()
			c := github.Client{HTTP: httpx.New(s.URL, "fictional-pat", time.Second, 1<<20)}
			c.HTTP.TokenEnv = "FICTIONAL_GITHUB_TOKEN"
			_, _, archiveErr := c.Archive(context.Background(), "example/charts", archiveSHA)
			_, treeErr := c.Tree(context.Background(), "example/charts", archiveSHA)
			got, want := archiveIssue(t, archiveErr), archiveIssue(t, treeErr)
			if got.Code != tc.code || got.ReasonCode != tc.reason {
				t.Fatalf("archive issue %s/%s, want %s/%s: %+v", got.Code, got.ReasonCode, tc.code, tc.reason, got)
			}
			if got.Code != want.Code || got.ReasonCode != want.ReasonCode || got.Stage != want.Stage || got.Repository != "example/charts" || got.Repository != want.Repository || !reflect.DeepEqual(got.RequiredAccess, want.RequiredAccess) {
				t.Fatalf("archive issue differs from the tree read:\narchive %+v\ntree    %+v", got, want)
			}
			if !strings.Contains(got.Reason, "GitHub source archive download (GET /repos/{owner}/{repo}/tarball/{ref})") {
				t.Fatalf("reason does not name the request: %q", got.Reason)
			}
			if got.Code == "access_denied" && (!strings.Contains(got.Reason, "FICTIONAL_GITHUB_TOKEN: provided") || !strings.Contains(got.Action, "SSO authorization")) {
				t.Fatalf("access denial lacks credential diagnostics: %+v", got)
			}
			if tc.status == http.StatusForbidden && got.Code == "access_denied" && !strings.Contains(got.Action, "required read access: Contents: read") {
				t.Fatalf("forbidden archive lacks the required permission: %q", got.Action)
			}
			if strings.Contains(got.Reason+got.Action, "fictional-pat") {
				t.Fatal("credential leaked into the issue")
			}
			if (originOf(archiveErr) == domain.FailureTransient) != tc.transient {
				t.Fatalf("failure origin %q, transient want %t", originOf(archiveErr), tc.transient)
			}
		})
	}
}

// A request that does not finish in time may pass on retry: a client timeout
// is a connection failure and a stage deadline keeps its cause, so preview
// reports a timeout. Both are transient.
func TestArchiveTimeoutIsTransient(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer s.Close()
	defer close(release)

	c := github.Client{HTTP: httpx.New(s.URL, "fictional-pat", 50*time.Millisecond, 1<<20)}
	_, _, err := c.Archive(context.Background(), "example/charts", archiveSHA)
	if issue := archiveIssue(t, err); issue.Code != "connection_failed" || issue.Repository != "example/charts" {
		t.Fatalf("client timeout: %+v", issue)
	}
	if origin := originOf(err); origin != domain.FailureTransient {
		t.Fatalf("client timeout origin %q", origin)
	}

	c = github.Client{HTTP: httpx.New(s.URL, "fictional-pat", time.Minute, 1<<20)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = c.Archive(ctx, "example/charts", archiveSHA)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stage deadline cause lost: %v", err)
	}
	if issue := preview.ContextualIssue(err, domain.ApplicationID{}, "", ""); issue.Code != "timeout" {
		t.Fatalf("stage deadline issue %+v", issue)
	}
	if origin := originOf(err); origin != domain.FailureTransient {
		t.Fatalf("stage deadline origin %q", origin)
	}
}

func chartArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "source/Chart.yaml", Typeflag: tar.TypeReg, Mode: 0600, Size: 11}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("name: chart"))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The API request carries the credential; the signed redirect download does
// not, its status is mapped like the API request's, and its URL stays out of
// the issue. Without a token no Authorization header is sent at all.
func TestArchiveRedirectDropsCredential(t *testing.T) {
	for _, token := range []string{"fictional-pat", ""} {
		downloadStatus := http.StatusOK
		var apiAuth []string
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method %s", r.Method)
			}
			if r.URL.Path == "/download" {
				if _, ok := r.Header["Authorization"]; ok {
					t.Error("credential forwarded to the archive redirect")
				}
				w.WriteHeader(downloadStatus)
				if downloadStatus == http.StatusOK {
					w.Write(chartArchive(t))
				}
				return
			}
			apiAuth = append(apiAuth, strings.Join(r.Header.Values("Authorization"), ","))
			http.Redirect(w, r, "/download?signature=fictional-signature", http.StatusFound)
		}))
		c := github.Client{HTTP: httpx.New(s.URL, token, time.Second, 1<<20)}
		c.HTTP.HTTP = s.Client()
		c.HTTP.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		files, _, err := c.Archive(context.Background(), "example/charts", archiveSHA)
		if err != nil || string(files["Chart.yaml"]) != "name: chart" {
			t.Fatalf("token %q: redirected archive files=%v err=%v", token, files, err)
		}
		downloadStatus = http.StatusServiceUnavailable
		_, _, err = c.Archive(context.Background(), "example/charts", archiveSHA)
		issue := archiveIssue(t, err)
		if issue.ReasonCode != "server_error" || issue.Repository != "example/charts" || strings.Contains(issue.Reason+issue.Action, "fictional-signature") || originOf(err) != domain.FailureTransient {
			t.Fatalf("token %q: redirect download failure %+v", token, issue)
		}
		s.Close()
		want := []string{"Bearer " + token, "Bearer " + token}
		if token == "" {
			want = []string{"", ""}
		}
		if !reflect.DeepEqual(apiAuth, want) {
			t.Fatalf("API request Authorization %q, want %q", apiAuth, want)
		}
	}
}
