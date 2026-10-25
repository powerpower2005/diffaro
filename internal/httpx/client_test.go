package httpx

import (
	"context"
	"diffaro/internal/domain"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSafeFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"denied", 403, `fictional-sensitive-error`, "access_denied"},
		{"missing", 404, `fictional-sensitive-error`, "not_found"},
		{"server", 500, `fictional-sensitive-error`, "backend_error"},
		{"invalid", 200, `fictional-sensitive-error`, "response_invalid"},
		{"too large", 200, strings.Repeat("x", 33), "limit_exceeded"},
		{"trailing", 200, `{} {}`, "response_invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("mutation attempted")
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer s.Close()
			var out any
			err := New(s.URL, "fictional-test-token", time.Second, 32).Get(context.Background(), "/", nil, &out)
			var f *domain.Failure
			if !errors.As(err, &f) || f.Issue.Code != tt.code {
				t.Fatalf("unexpected failure: %v", err)
			}
			if strings.Contains(err.Error(), "fictional-sensitive-error") {
				t.Fatal("backend body leaked")
			}
		})
	}
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	var out any
	if New(source.URL, "fictional-test-token", time.Second, 1024).Get(context.Background(), "/", nil, &out) == nil {
		t.Fatal("redirect accepted")
	}
	if called {
		t.Fatal("redirect followed")
	}
}

// TestGetAndOpenShareRequestRules protects the request rules that Get and
// OpenFor share: the credential, the User-Agent of this build, and a transport
// failure cause without the URL, whose query may carry values.
func TestGetAndOpenShareRequestRules(t *testing.T) {
	var agents []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fictional-test-token" {
			t.Error("credential missing")
		}
		agents = append(agents, r.Header.Get("User-Agent"))
		fmt.Fprint(w, `{}`)
	}))
	client := New(s.URL, "fictional-test-token", time.Second, 1024)
	var out any
	if err := client.Get(context.Background(), "/", nil, &out); err != nil {
		t.Fatal(err)
	}
	resp, err := client.OpenFor(context.Background(), "/", "", RequestDiagnostic{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 2 || agents[0] != "diffaro/"+userAgentVersion || agents[1] != agents[0] {
		t.Fatalf("User-Agent %q", agents)
	}
	s.Close()
	err = client.Get(context.Background(), "/", url.Values{"q": {"fictional-query-value"}}, &out)
	_, openErr := client.OpenFor(context.Background(), "/?q=fictional-query-value", "", RequestDiagnostic{})
	for _, err := range []error{err, openErr} {
		var urlError *url.Error
		if err == nil || errors.As(err, &urlError) {
			t.Fatalf("transport failure kept the URL: %v", err)
		}
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out any
	err := New("http://127.0.0.1:1", "", time.Second, 1024).Get(ctx, "/", nil, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cause lost: %v", err)
	}
}

func TestRateLimitIsDistinctFromAccessDenial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		code    string
		reason  string
	}{
		{"primary quota", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000000"}, "rate_limited", "quota resets at 2027-01-15T08:00:00Z"},
		{"secondary limit", 403, map[string]string{"Retry-After": "60"}, "rate_limited", "retry after 60s"},
		{"too many requests", 429, nil, "rate_limited", "HTTP 429 Too Many Requests"},
		{"plain forbidden", 403, map[string]string{"X-RateLimit-Remaining": "4999"}, "access_denied", "request was forbidden"},
		{"unparsable reset", 429, map[string]string{"X-RateLimit-Reset": "fictional-sensitive-header"}, "rate_limited", "rate limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "fictional-sensitive-error")
			}))
			defer s.Close()
			var out any
			err := New(s.URL, "fictional-test-token", time.Second, 1024).GetFor(context.Background(), "/", nil, &out, RequestDiagnostic{Service: "GitHub", Operation: "revision lookup", Route: "/repos/{owner}/{repo}/commits/{ref}", Permission: "Contents: read"})
			var f *domain.Failure
			if !errors.As(err, &f) || f.Issue.Code != tc.code || !strings.Contains(f.Issue.Reason, tc.reason) {
				t.Fatalf("unexpected failure: %v", err)
			}
			if tc.code == "rate_limited" && (f.Issue.ReasonCode != "" || f.Issue.RequiredAccess != nil || strings.Contains(f.Issue.Action, "required read access") || !strings.Contains(f.Issue.Action, "quota reset")) {
				t.Fatalf("rate limit reported as a permission problem: %+v", f.Issue)
			}
			if strings.Contains(err.Error()+f.Issue.Action, "fictional-sensitive") {
				t.Fatal("backend body or header leaked")
			}
		})
	}
}
