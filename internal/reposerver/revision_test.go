package reposerver

import (
	"context"
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

func TestResolveSourceRevisionUsesScopedCredentials(t *testing.T) {
	snapshot, app, project := fixture()
	app.Spec["sources"] = []any{app.Source(), map[string]any{"repoURL": "https://example.com/values", "ref": "values", "targetRevision": "main"}}
	delete(app.Spec, "source")
	original := domain.CloneSpec(app.Spec)
	calls := 0
	client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot}
	client.resolve = func(_ context.Context, request map[string]any) (Response, error) {
		calls++
		if request["ambiguousRevision"] != "feature" || request["noRevisionCache"] != true {
			t.Fatal("mutable reference was not resolved freshly")
		}
		repo := request["repo"].(map[string]any)
		if repo["repo"] != repository || repo["password"] != "fictional-repo-password" {
			t.Fatal("trusted repository credentials missing")
		}
		want := map[string]any{"spec": map[string]any{"source": map[string]any{"repoURL": repository}}}
		if !reflect.DeepEqual(request["app"], want) {
			t.Fatal("resolver received unnecessary or untrusted Application fields")
		}
		return Response{Revision: commit}, nil
	}
	got, err := client.ResolveSourceRevision(context.Background(), app, repository, "feature")
	if err != nil || got != commit || calls != 1 {
		t.Fatalf("resolve: %q %v", got, err)
	}
	if !reflect.DeepEqual(app.Spec, original) {
		t.Fatal("Application spec mutated")
	}
	_, err = client.ResolveSourceRevision(context.Background(), app, "https://example.com/values", "main")
	if issueCode(err) != "access_denied" || calls != 1 {
		t.Fatal("unbound source reached the repository RPC")
	}
	_, err = client.ResolveSourceRevision(context.Background(), app, repository, "other-branch")
	if issueCode(err) != "configuration_invalid" || calls != 1 {
		t.Fatal("unrequested reference reached the repository RPC")
	}
}

func TestResolveSourceRevisionRejectsPolicyDriftAndInvalidRevision(t *testing.T) {
	for _, scenario := range []string{"denied", "drift", "not-sha"} {
		t.Run(scenario, func(t *testing.T) {
			snapshot, app, project := fixture()
			calls := 0
			client := &Client{snapshot: snapshot, API: fakeAPI(t, func(n int) map[string]any {
				copy := domain.CloneSpec(project)
				if scenario == "denied" || (scenario == "drift" && n > 1) {
					copy["sourceRepos"] = []any{}
				}
				return copy
			})}
			client.resolve = func(context.Context, map[string]any) (Response, error) {
				calls++
				if scenario == "not-sha" {
					return Response{Revision: "main"}, nil
				}
				return Response{Revision: commit}, nil
			}
			_, err := client.ResolveSourceRevision(context.Background(), app, repository, "feature")
			want := map[string]string{"denied": "access_denied", "drift": "render_context_changed", "not-sha": "revision_invalid"}[scenario]
			if issueCode(err) != want {
				t.Fatalf("got %v, wanted %s", err, want)
			}
			if scenario == "denied" && calls != 0 {
				t.Fatal("denied source was accessed")
			}
		})
	}
}
