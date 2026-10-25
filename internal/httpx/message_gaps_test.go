package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
)

// g67aStatusError serves one response and returns the StatusError cause and
// the failure that Get reports for it.
func g67aStatusError(t *testing.T, status int, body string) (*StatusError, *domain.Failure) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer s.Close()
	var out any
	err := New(s.URL, "fictional-test-token", time.Second, 1<<10).Get(context.Background(), "/", nil, &out)
	var statusErr *StatusError
	var failure *domain.Failure
	if !errors.As(err, &statusErr) || !errors.As(err, &failure) {
		t.Fatalf("status cause lost: %v", err)
	}
	return statusErr, failure
}

// A server error keeps the gRPC-gateway message on the cause as one bounded
// line for adapters to match, and never writes it into the reported failure.
// Other statuses and non-JSON bodies keep no message.
func TestServerErrorKeepsGatewayMessageOnlyOnCause(t *testing.T) {
	const message = "fictional-chart/api: app path does not exist"
	statusErr, failure := g67aStatusError(t, http.StatusInternalServerError, `{"code": 2, "message": "fictional-chart/api:\n  app   path does not exist"}`)
	if statusErr.Message != message {
		t.Fatalf("gateway message not kept as one line: %q", statusErr.Message)
	}
	if strings.Contains(failure.Issue.Reason, "fictional-chart") || strings.Contains(failure.Issue.Action, "fictional-chart") || strings.Contains(failure.Error(), "fictional-chart") {
		t.Fatal("gateway message written into the reported failure", failure)
	}

	statusErr, _ = g67aStatusError(t, http.StatusBadGateway, `{"message": "`+strings.Repeat("é", 400)+`"}`)
	if got := []rune(statusErr.Message); len(got) != 301 || string(got[300:]) != "…" {
		t.Fatalf("long gateway message not bounded to 300 characters: %d runes", len(got))
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-JSON server error", http.StatusInternalServerError, "fictional-chart plain text"},
		{"forbidden", http.StatusForbidden, `{"message": "fictional-chart denied"}`},
		{"missing", http.StatusNotFound, `{"message": "fictional-chart missing"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if statusErr, _ := g67aStatusError(t, tc.status, tc.body); statusErr.Message != "" {
				t.Fatalf("message kept for %s: %q", tc.name, statusErr.Message)
			}
		})
	}
}
