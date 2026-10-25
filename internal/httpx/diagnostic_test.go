package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
)

func TestRequestDiagnosticsDistinguishAuthenticationAndReadAccess(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status                 int
		token                  string
		wantReason, wantAction string
	}{
		{"missing credential", 401, "", "DIFFARO_TEST_TOKEN: not set or empty", "set DIFFARO_TEST_TOKEN in the process"},
		{"empty credential", 401, " ", "DIFFARO_TEST_TOKEN: not set or empty", ".env files are not loaded automatically"},
		{"rejected credential", 401, "fictional-sensitive-token", "authentication was rejected", "expiration, revocation"},
		{"forbidden request", 403, "fictional-sensitive-token", "request was forbidden", "required read access: projects/get"},
		{"anonymous forbidden", 403, "", "DIFFARO_TEST_TOKEN: not set or empty", "set DIFFARO_TEST_TOKEN"},
		{"hidden object", 404, "fictional-sensitive-token", "HTTP 404 Not Found", "private objects may be hidden"},
		{"backend down", 503, "fictional-sensitive-token", "HTTP 503 Service Unavailable", "gateway routing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("mutation attempted")
				}
				w.Header().Set("X-Request-Id", "fictional-sensitive-header")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("fictional-sensitive-body"))
			}))
			defer server.Close()
			client := New(server.URL, tc.token, time.Second, 1024)
			client.TokenEnv = "DIFFARO_TEST_TOKEN"
			var out any
			err := client.GetFor(context.Background(), "/fictional-sensitive-path", url.Values{"revision": {"fictional-sensitive-query"}}, &out, RequestDiagnostic{Service: "Argo CD", Instance: "test", Operation: "AppProject lookup", Route: "/api/v1/projects/{project}", Permission: "projects/get"})
			var failure *domain.Failure
			var status *StatusError
			if !errors.As(err, &failure) || !errors.As(err, &status) || status.StatusCode != tc.status {
				t.Fatalf("lost typed HTTP cause: %v", err)
			}
			if (tc.status == 401 || tc.status == 403) && failure.Issue.Code != "access_denied" {
				t.Fatal("access denial code changed")
			}
			for _, text := range []string{"Argo CD instance test AppProject lookup", "GET /api/v1/projects/{project}", tc.wantReason} {
				if !strings.Contains(failure.Issue.Reason, text) {
					t.Fatalf("missing diagnostic %q: %s", text, failure.Issue.Reason)
				}
			}
			if !strings.Contains(failure.Issue.Action, tc.wantAction) {
				t.Fatalf("missing action: %s", failure.Issue.Action)
			}
			if strings.Contains(err.Error()+failure.Issue.Action, "fictional-sensitive") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("transport diagnostic leaked credentials, URL, path, query, headers or body")
			}
		})
	}
}

func TestDiagnosticDoesNotEchoInvalidEnvironmentVariableName(t *testing.T) {
	client := New("", "", time.Second, 1024)
	client.TokenEnv = "Authorization: fictional-sensitive-env\n"
	err := client.statusFailure(401, "", "access_denied", "permission")
	var failure *domain.Failure
	if !errors.As(err, &failure) || strings.Contains(err.Error()+failure.Issue.Action, "fictional-sensitive") || !strings.Contains(failure.Issue.Action, "provide a token in the profile") {
		t.Fatal("invalid environment variable name echoed")
	}
}

func TestAdapterDiagnosticPreservesCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out any
	err := New("http://127.0.0.1:1", "", time.Second, 1024).GetFor(ctx, "/", nil, &out, RequestDiagnostic{Service: "Argo CD", Operation: "Application inventory lookup", Route: "/api/v1/applications"})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Application inventory lookup") {
		t.Fatal("adapter context lost the cancellation cause")
	}
}

func TestAccessReasonCodesPreserveAggregateCodeAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		status      int
		token, want string
	}{
		{401, "", "credential_missing"},
		{401, " \t", "credential_missing"},
		{401, "fictional-token", "authentication_rejected"},
		{403, "", "credential_missing"},
		{403, "fictional-token", "request_forbidden"},
		// Backend errors name whether a retry may help.
		{404, "fictional-token", "unexpected_status"},
		{503, "fictional-token", "server_error"},
	} {
		code := "backend_error"
		if tc.status == 401 || tc.status == 403 {
			code = "access_denied"
		}
		err := New("", tc.token, time.Second, 1024).statusFailure(tc.status, "", code, "permission")
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != code || failure.Issue.ReasonCode != tc.want {
			t.Fatalf("status %d: classification changed: %v", tc.status, err)
		}
		if strings.Contains(failure.Issue.Reason, "valid token") {
			t.Fatal("supplied credential was treated as proven valid")
		}
	}
}

func TestRequiredAccessUsesClientCredentialLabelAndCopiesMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	for _, env := range []string{"DIFFARO_TEST_TOKEN", "Authorization: fictional-private-value\n"} {
		client := New(server.URL, "fictional-private-value", time.Second, 1024)
		client.TokenEnv = env
		required := &domain.AccessRequirement{System: "argocd-rbac", Resource: "projects", Action: "get", Target: "demo", Object: "demo", CredentialEnv: "fictional-private-value"}
		err := client.GetFor(context.Background(), "/api/v1/projects/demo", nil, new(any), RequestDiagnostic{Service: "Argo CD", Instance: "test", RequiredAccess: required})
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.RequiredAccess == nil {
			t.Fatal("requirement missing")
		}
		access := failure.Issue.RequiredAccess
		want := ""
		if env == "DIFFARO_TEST_TOKEN" {
			want = env
		}
		if access.CredentialEnv != want || access.Instance != "test" || access == required || required.Instance != "" || required.CredentialEnv != "fictional-private-value" {
			t.Fatal("unsafe label or shared metadata mutation")
		}
	}
}

func TestConfiguredTokenDiagnosticDoesNotClaimEnvironmentSource(t *testing.T) {
	client := New("", "fictional-sensitive-token", time.Second, 1024)
	err := client.statusFailure(401, "", "access_denied", "permission")
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		t.Fatal("missing structured failure")
	}
	if !strings.Contains(failure.Issue.Reason, "configured token: provided") || !strings.Contains(failure.Issue.Action, "profile token or explicit credential flag") || strings.Contains(failure.Issue.Reason+failure.Issue.Action, "fictional-") || strings.Contains(failure.Issue.Reason, "environment variable") {
		t.Fatal("credential source is misleading or unsafe")
	}
}
