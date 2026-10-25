package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
	"diffaro/internal/revision"
)

func TestMultiSourceDesiredAnalysisSkipPreservesFailureBoundary(t *testing.T) {
	for _, scenario := range []string{"new", "modified", "denied", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			stored := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "child"}, Spec: map[string]any{"project": "demo", "sources": []any{
				map[string]any{"repoURL": "https://example.com/charts.git", "path": "chart", "targetRevision": strings.Repeat("a", 40), "helm": map[string]any{"releaseName": "stable"}},
				map[string]any{"repoURL": "https://example.com/values.git", "ref": "values", "targetRevision": strings.Repeat("b", 40)},
			}}}
			var renders atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("production mutation attempted")
				}
				if strings.HasSuffix(r.URL.Path, "/manifests") {
					renders.Add(1)
				}
				switch scenario {
				case "new":
					http.NotFound(w, r)
				case "denied":
					http.Error(w, "denied", http.StatusForbidden)
				case "unavailable":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "child", "namespace": "argocd"}, "spec": stored.Spec})
				}
			}))
			defer server.Close()
			desired := domain.Application{ID: stored.ID, Spec: domain.CloneSpec(stored.Spec)}
			desired.Spec["sources"].([]any)[0].(map[string]any)["helm"] = map[string]any{"releaseName": "new-release"}
			plan, err := revision.PlanSources(context.Background(), desired, "https://example.com/charts.git", strings.Repeat("c", 40), nil)
			if err != nil {
				t.Fatal(err)
			}
			client := Client{Instance: "test", HTTP: httpx.New(server.URL, "", time.Second, 1<<20)}
			_, err = client.RenderDesiredSources(context.Background(), desired, plan)
			var skipped *domain.RenderSkipped
			wantSkip := scenario == "new" || scenario == "modified"
			if err == nil || errors.As(err, &skipped) != wantSkip || renders.Load() != 0 {
				t.Fatalf("multi-source analysis skip boundary changed: %v", err)
			}
		})
	}
}

func TestMultiSourceManifestQueryPreservesPositionsAndDetectsDrift(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "changed-during-render"}[drift], func(t *testing.T) {
			fixed, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "shop"}, Spec: map[string]any{"project": "demo", "sources": []any{
				map[string]any{"repoURL": "https://example.com/team/chart", "path": "chart", "targetRevision": fixed, "helm": map[string]any{"valueFiles": []any{"$values/prod.yaml"}}},
				map[string]any{"repoURL": "https://example.com/team/values", "ref": "values", "targetRevision": "main"},
			}}}
			plan, err := revision.PlanSources(context.Background(), app, "https://example.com/team/values", head, nil)
			if err != nil {
				t.Fatal(err)
			}
			var lookups, renders atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fictional-token" {
					t.Error("unexpected request method or credentials")
				}
				switch r.URL.Path {
				case "/api/v1/applications/shop":
					spec := domain.CloneSpec(app.Spec)
					if lookups.Add(1) > 1 && drift {
						spec["sources"].([]any)[0].(map[string]any)["path"] = "changed"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "shop", "namespace": "argocd"}, "spec": spec})
				case "/api/v1/applications/shop/manifests":
					renders.Add(1)
					q := r.URL.Query()
					if !reflect.DeepEqual(q["sourcePositions"], []string{"1", "2"}) || !reflect.DeepEqual(q["revisions"], []string{fixed, head}) || q.Get("revision") != "" {
						t.Error("per-source revision mapping lost")
					}
					_, _ = w.Write([]byte(`{"manifests":["apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop"]}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-token", time.Second, 1<<20)}
			manifests, err := client.RenderSources(context.Background(), app, plan)
			if drift {
				if err == nil || manifests != nil {
					t.Fatal("drift returned successful manifests")
				}
			} else if err != nil || len(manifests) != 1 {
				t.Fatalf("render failed: %v", err)
			}
			if lookups.Load() != 2 || renders.Load() != 1 {
				t.Fatal("context not checked before and after render")
			}
			plan[0].Source["path"] = "different-chart"
			if _, err := client.RenderSources(context.Background(), app, plan); err == nil {
				t.Fatal("plan changed inputs other than revision")
			}
			if renders.Load() != 1 {
				t.Fatal("invalid plan reached backend")
			}
		})
	}
}

func TestWithoutSourceRevisionsPreservesOrderAndSettings(t *testing.T) {
	spec := map[string]any{"sources": []any{map[string]any{"ref": "values", "targetRevision": "main", "repoURL": "repo-a"}, map[string]any{"targetRevision": "main", "repoURL": "repo-b", "helm": map[string]any{"releaseName": "shop"}}}}
	before := domain.CloneSpec(spec)
	stripped := withoutSourceRevisions(spec)
	if !reflect.DeepEqual(spec, before) {
		t.Fatal("stored spec mutated")
	}
	sources := stripped["sources"].([]any)
	if domain.String(sources[0].(map[string]any), "ref") != "values" || sources[1].(map[string]any)["helm"] == nil {
		t.Fatal("source inputs lost")
	}
	for _, source := range sources {
		if _, ok := source.(map[string]any)["targetRevision"]; ok {
			t.Fatal("revision not removed")
		}
	}
}
