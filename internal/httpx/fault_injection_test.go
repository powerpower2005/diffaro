package httpx

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
)

// TestInjectedTransportFaultsAreClassified injects the provider faults an
// operator's network or backend can produce. None may reach the user as
// internal_error, and every failure keeps its creation site, HTTP
// status and route template without the URL, query or body.
func TestInjectedTransportFaultsAreClassified(t *testing.T) {
	const canary = "fictional-canary-7f3a"
	for _, tt := range []struct {
		name    string
		tls     bool
		handler http.HandlerFunc
		code    string
		status  int
		chain   bool
		timeout time.Duration
		// reason is the cause a user acts on.
		reason string
	}{
		{name: "connection reset", handler: func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}, code: domain.CodeConnectionFailed, chain: true, reason: domain.ReasonConnectionClosed},
		{name: "untrusted TLS certificate", tls: true, handler: func(http.ResponseWriter, *http.Request) {}, code: domain.CodeConnectionFailed, chain: true, reason: domain.ReasonTLSUntrusted},
		{name: "slow headers", handler: func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, code: domain.CodeConnectionFailed, chain: true, reason: domain.ReasonTimeout, timeout: 300 * time.Millisecond},
		{name: "truncated body", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "200")
			w.Write([]byte(`{"items": ["` + canary))
		}, code: domain.CodeResponseInvalid, chain: true},
		{name: "invalid JSON", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"items": ` + canary))
		}, code: domain.CodeResponseInvalid, chain: true},
		{name: "HTML server error", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>" + canary + "</html>"))
		}, code: domain.CodeBackendError, status: http.StatusBadGateway, reason: domain.ReasonServerError},
		{name: "slow body", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"items": [`))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, code: domain.CodeResponseInvalid, chain: true, timeout: 300 * time.Millisecond},
		{name: "redirect", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example.invalid/"+canary, http.StatusFound)
		}, code: domain.CodeBackendError, status: http.StatusFound, reason: domain.ReasonRedirected},
		{name: "unauthorized", handler: status(http.StatusUnauthorized, canary), code: domain.CodeAccessDenied, status: http.StatusUnauthorized, reason: domain.ReasonAuthenticationRejected},
		{name: "forbidden", handler: status(http.StatusForbidden, canary), code: domain.CodeAccessDenied, status: http.StatusForbidden, reason: domain.ReasonRequestForbidden},
		{name: "not found", handler: status(http.StatusNotFound, canary), code: domain.CodeNotFound, status: http.StatusNotFound},
		{name: "too many requests", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(canary))
		}, code: domain.CodeRateLimited, status: http.StatusTooManyRequests},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(tt.handler)
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			if tt.tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			// Only timeout scenarios need a short deadline. TLS verification
			// must have time to finish even under parallel race-test load.
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			client := New(server.URL, "fictional-test-token", timeout, 1<<20)
			var out any
			err := client.GetFor(context.Background(), "/api/v1/"+canary, url.Values{"q": {canary}}, &out, RequestDiagnostic{Service: "Fictional", Operation: "read", Route: "/api/v1/{name}"})
			var failure *domain.Failure
			if !errors.As(err, &failure) {
				t.Fatalf("error is not classified: %T %v", err, err)
			}
			issue := failure.Issue
			if issue.Code != tt.code || issue.Code == domain.CodeInternalError || issue.ReasonCode != tt.reason {
				t.Fatalf("code = %s/%s, want %s/%s", issue.Code, issue.ReasonCode, tt.code, tt.reason)
			}
			if host := strings.TrimPrefix(strings.TrimPrefix(server.URL, "https://"), "http://"); issue.Code == domain.CodeConnectionFailed && !strings.Contains(issue.Reason, "API request to "+host) {
				t.Fatalf("connection failure does not name the host: %q", issue.Reason)
			}
			d := issue.Diagnostic
			if d == nil || !strings.HasPrefix(d.Site, "httpx/") || d.Route != "/api/v1/{name}" || d.HTTPStatus != tt.status {
				t.Fatalf("diagnostic = %+v", d)
			}
			if tt.chain && len(d.Chain) == 0 {
				t.Fatalf("transport fault lost its cause chain: %+v", d)
			}
			encoded, _ := json.Marshal(issue)
			if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), server.URL) || strings.Contains(string(encoded), "fictional-test-token") {
				t.Fatalf("issue leaked request or response content: %s", encoded)
			}
		})
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(body))
	}
}

// A refused connection and an unresolvable host name are told apart from
// other transport faults, with an action for each.
func TestTransportCausesNameTheirAction(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	closed := server.URL
	server.Close()
	err := New(closed, "", time.Second, 1<<20).Get(context.Background(), "/api", nil, new(any))
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.ReasonCode != domain.ReasonConnectionRefused || !strings.Contains(failure.Issue.Action, "port") {
		t.Fatalf("refused connection = %+v", failure)
	}
	for _, tc := range []struct {
		err    error
		reason string
		action string
	}{
		{&net.DNSError{Err: "no such host", Name: "argocd.example.invalid", IsNotFound: true}, domain.ReasonDNSFailed, "host name"},
		{x509.UnknownAuthorityError{}, domain.ReasonTLSUntrusted, "trust store"},
		{errors.New("unrecognized"), "", "check connectivity, TLS and timeout settings"},
	} {
		reason, _, action := transportCause(tc.err)
		if reason != tc.reason || !strings.Contains(action, tc.action) {
			t.Fatalf("%T: reason %q action %q", tc.err, reason, action)
		}
	}
}
