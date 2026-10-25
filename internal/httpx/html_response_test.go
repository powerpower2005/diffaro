package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
)

// A 200 response that is an HTML page, as an SSO or login gateway serves, names
// the page and the endpoint setting to check; the media type is kept as a
// diagnostic fact without parameters or body text. Another
// undecodable body keeps the generic action.
func TestHTMLResponseNamesTheLoginGateway(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, mediaType, reason, action string
	}{
		{"login page", "text/html; charset=fictional-secret", "text/html", "backend returned an HTML page instead of JSON", "git.apiURL names the API itself"},
		{"broken JSON", "application/json", "application/json", "backend response is not valid JSON", "check backend compatibility"},
		{"unbounded media type", "application/" + strings.Repeat("x", 100), "", "backend response is not valid JSON", "check backend compatibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte("<html>fictional-secret sign in</html>"))
			}))
			defer server.Close()
			var out map[string]any
			err := New(server.URL, "", time.Second, 1<<20).GetFor(context.Background(), "/api/v1/applications/api", nil, &out, RequestDiagnostic{Service: "Argo CD", Instance: "one", Operation: "Application lookup", Route: "/api/v1/applications/{application}"})
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeResponseInvalid || failure.Issue.Stage != "decode" || !strings.Contains(failure.Issue.Reason, tc.reason) || !strings.Contains(failure.Issue.Action, tc.action) {
				t.Fatalf("issue = %+v", failure)
			}
			if failure.Issue.Diagnostic == nil || failure.Issue.Diagnostic.ContentType != tc.mediaType || failure.Issue.Diagnostic.Route != "/api/v1/applications/{application}" {
				t.Fatalf("diagnostic = %+v, want content type %q", failure.Issue.Diagnostic, tc.mediaType)
			}
			encoded, _ := json.Marshal(failure.Issue)
			if strings.Contains(string(encoded), "fictional") {
				t.Fatalf("response text or header parameters leaked: %s", encoded)
			}
		})
	}
}
