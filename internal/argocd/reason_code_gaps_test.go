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

// Argo CD's "app path does not exist" carries chart_path_missing in the JSON
// issue, so reports title it as a missing chart path.
func TestMissingAppPathReasonCode(t *testing.T) {
	spec := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://github.com/example/charts.git", "path": "charts/removed", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster"}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifests") {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "charts/removed: app path does not exist"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "api", "namespace": "argocd"}, "spec": spec})
	}))
	defer s.Close()
	c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}, Spec: domain.CloneSpec(spec)}
	_, err := c.Render(context.Background(), app, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.ReasonCode != "chart_path_missing" {
		t.Fatalf("unexpected failure: %v", err)
	}
	data, _ := json.Marshal(failure.Issue)
	if !strings.Contains(string(data), `"reasonCode":"chart_path_missing"`) {
		t.Fatalf("reason code missing from JSON issue: %s", data)
	}
}
