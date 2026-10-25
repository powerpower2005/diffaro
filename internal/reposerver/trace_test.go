package reposerver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// A traced repo-server render records its Application drift reads, the
// binding, AppProject read and authorization, the RPC and its repeat
// with the digest of the manifests they returned, and the
// AppProject drift read, in that order.
// Without a trace the render is unchanged.
func TestRenderTraceRecordsRepoServerSteps(t *testing.T) {
	snapshot, app, project := fixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/" + app.ID.Name:
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": app.ID.Name, "namespace": app.ID.Namespace}, "spec": app.Spec})
		case "/api/v1/projects/demo":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": project})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{API: &argocd.Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-api-token", time.Second, 1<<20)}, snapshot: snapshot, invoke: func(context.Context, map[string]any) (Response, error) {
		return Response{Revision: commit, SourceType: "Helm", Manifests: []string{"rendered"}}, nil
	}}
	untraced, err := client.Render(context.Background(), app, commit)
	if err != nil {
		t.Fatal(err)
	}
	trace := domain.NewTrace()
	traced, err := client.Render(domain.WithTrace(context.Background(), trace), app, commit)
	if err != nil || strings.Join(traced, "") != strings.Join(untraced, "") {
		t.Fatalf("trace changed the render: %v %v", traced, err)
	}
	steps, _ := trace.Steps()
	var got []string
	for _, step := range steps {
		if step.Phase == "http" {
			continue
		}
		if step.Outcome != "ok" {
			t.Fatalf("step %s/%s: %s", step.Phase, step.Step, step.Outcome)
		}
		got = append(got, step.Phase+"/"+step.Step)
		if step.Phase == "render" && step.Digest != domain.DigestJSON([]string{"rendered"}) {
			t.Fatalf("RPC step lacks the manifests digest: %+v", step)
		}
	}
	want := "lookup/drift-before project/binding project/spec project/authorize render/rpc render/rpc project/drift-after lookup/drift-after"
	if strings.Join(got, " ") != want {
		t.Fatalf("steps:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
}
