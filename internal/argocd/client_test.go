package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

func TestDesiredRenderRejectsDifferentContextBeforeManifestRequest(t *testing.T) {
	for _, field := range []string{"valueFiles", "values", "valuesObject", "parameters", "fileParameters", "releaseName", "destination", "project", "unknownFutureInput"} {
		t.Run(field, func(t *testing.T) {
			stored := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster"}}
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("mutation attempted")
				}
				if r.URL.Path != "/api/v1/applications/child" {
					calls.Add(1)
					http.Error(w, "unexpected render", 500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "child", "namespace": "argocd"}, "spec": stored})
			}))
			defer s.Close()
			desired := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "child"}, Spec: domain.CloneSpec(stored)}
			switch field {
			case "destination":
				desired.Spec[field] = map[string]any{"name": "another-cluster"}
			case "project":
				desired.Spec[field] = "another-project"
			case "unknownFutureInput":
				desired.Spec[field] = map[string]any{"setting": "different"}
			default:
				desired.Source()["helm"].(map[string]any)[field] = "different"
			}
			c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
			_, err := c.RenderDesired(context.Background(), desired, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			var failure *domain.Failure
			var skipped *domain.RenderSkipped
			if !errors.As(err, &skipped) || skipped.Reason == "" || !errors.As(err, &failure) || failure.Issue.Code != "desired_spec_unavailable" || calls.Load() != 0 {
				t.Fatal("desired render context was replaced by installed inputs")
			}
		})
	}
}

// upstreamApplicationGet mirrors Argo CD's getAppEnforceRBAC: a missing
// Application is 403 unless the query names a project, and a project that
// differs from the stored Application is denied.
func upstreamApplicationGet(stored map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "mutation attempted", http.StatusMethodNotAllowed)
			return
		}
		project := r.URL.Query().Get("project")
		switch {
		case stored == nil && project == "":
			http.Error(w, `{"message":"permission denied"}`, http.StatusForbidden)
		case stored == nil:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case project != "" && project != stored["project"]:
			http.Error(w, `{"message":"permission denied"}`, http.StatusForbidden)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "child", "namespace": "argocd"}, "spec": stored})
		}
	}
}

func TestDesiredRenderOfNewChildIsSkippedNotDenied(t *testing.T) {
	spec := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster"}}
	for _, tc := range []struct {
		name   string
		stored map[string]any
		reason string
	}{
		{"new child", nil, "not_found"},
		{"stored in another project", map[string]any{"project": "other", "source": spec["source"], "destination": spec["destination"]}, "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(upstreamApplicationGet(tc.stored))
			defer s.Close()
			c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
			desired := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "child"}, Spec: domain.CloneSpec(spec)}
			for name, render := range map[string]func() error{
				"single source": func() error {
					_, err := c.RenderDesired(context.Background(), desired, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
					return err
				},
				"source plan": func() error {
					_, err := c.RenderDesiredSources(context.Background(), desired, nil)
					return err
				},
			} {
				err := render()
				var failure *domain.Failure
				var skipped *domain.RenderSkipped
				if !errors.As(err, &skipped) || !errors.As(err, &failure) || failure.Issue.Code != "desired_spec_unavailable" || !strings.Contains(skipped.Reason, tc.reason) {
					t.Fatalf("%s: err=%v, want desired_spec_unavailable skip mentioning %q", name, err, tc.reason)
				}
				// Local Helm needs no Kubernetes access, so it is recommended
				// before repo-server.
				for _, text := range []string{skipped.Reason, failure.Issue.Action} {
					if l, r := strings.Index(text, "local Helm"), strings.Index(text, "repo-server"); l < 0 || r < 0 || l > r {
						t.Fatalf("%s: %q must recommend local Helm before repo-server", name, text)
					}
				}
			}
		})
	}
}
