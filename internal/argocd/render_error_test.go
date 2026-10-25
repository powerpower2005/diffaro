package argocd

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
	"diffaro/internal/httpx"
)

// Argo CD answers a render failure with HTTP 500 and the repo-server message.
// A known render cause gets the reason code repo-server and local Helm give
// it, as a deterministic failure; the message, which can quote
// values, is withheld. Another server error stays a backend error.
func TestRenderServerErrorsNameMissingPathAndWithholdOtherMessages(t *testing.T) {
	spec := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "path": "charts/removed", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster"}}
	// httpx keeps the first 300 characters of the message, so a long helm
	// command line can cut off the template location.
	const command = "Manifest generation error (cached): `helm template . --set password=fictional-value` failed exit status 1: "
	for _, tc := range []struct {
		name, message, code, reasonCode, stage string
		want, absent                           []string
	}{
		{
			"missing app path", "charts/removed: app path does not exist", "not_found", "chart_path_missing", "render",
			[]string{`source path "charts/removed" does not exist at this revision (HTTP 500)`, "app path does not exist"},
			[]string{"gateway routing"},
		},
		{
			"template location", command + "Error: template: api/templates/deployment.yaml:12:20: executing \"api/templates/deployment.yaml\" at <.Values.image.tag>: nil pointer evaluating interface {}.tag", "render_failed", "template_failed", "render",
			[]string{"template api/templates/deployment.yaml:12 failed: a nil value was referenced", "(HTTP 500)", "helm template on the chart"},
			[]string{"fictional-value", "gateway routing"},
		},
		{
			// Helm template errors can carry values, so they stay withheld.
			"template failure without location", "helm template failed: password fictional-value is too short", "render_failed", "template_failed", "render",
			[]string{"Helm template rendering failed in Argo CD", "HTTP 500"},
			[]string{"fictional-value", "gateway routing"},
		},
		{
			"missing value file", command + "Error: open /tmp/_argocd-repo/values-fictional-value.yaml: no such file or directory", "render_failed", "value_file_missing", "render",
			[]string{"value file or file parameter does not exist"},
			[]string{"fictional-value"},
		},
		{
			"other server message", "rpc error: code = Unavailable desc = upstream fictional-value unavailable", "backend_error", "server_error", "connection",
			[]string{"HTTP 500", "gateway routing"},
			[]string{"fictional-value"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/manifests") {
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.message, "code": 2, "message": tc.message})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "api", "namespace": "argocd"}, "spec": spec})
			}))
			defer s.Close()
			c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
			app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}, Spec: domain.CloneSpec(spec)}
			_, err := c.Render(context.Background(), app, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != tc.code || failure.Issue.ReasonCode != tc.reasonCode || failure.Issue.Stage != tc.stage {
				t.Fatalf("unexpected failure: %v", err)
			}
			text := failure.Issue.Reason + " " + failure.Issue.Action
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %q", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("unexpected %q in %q", absent, text)
				}
			}
		})
	}
}
