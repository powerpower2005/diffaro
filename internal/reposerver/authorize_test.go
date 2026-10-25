package reposerver

import (
	"context"
	"errors"
	"testing"

	"diffaro/internal/argopolicy"
)

// The repo-server and local Helm backends must reach the same AppProject
// decision; local Helm calls argopolicy.Authorize directly.
func TestRepoServerProjectDecisionMatchesLocalHelm(t *testing.T) {
	for _, tc := range []struct {
		name, namespace  string
		sourceRepos      []any
		sourceNamespaces []any
		wantRender       bool
	}{
		{"controller namespace", "argocd", []any{"*"}, nil, true},
		{"explicit repository deny", "argocd", []any{"*", "!" + repository}, nil, false},
		{"exact namespace", "team-a", []any{repository}, []any{"team-a"}, true},
		{"namespace glob", "team-a", []any{repository}, []any{"team-*"}, true},
		{"namespace wildcard", "team-a", []any{repository}, []any{"*"}, true},
		{"namespace glob without match", "other", []any{repository}, []any{"team-*"}, false},
		{"namespace regexp", "team-a", []any{repository}, []any{"/^team-.*$/"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, app, project := fixture()
			app.ID.Namespace, snapshot.Bindings[0].ApplicationNamespace = tc.namespace, tc.namespace
			project["sourceRepos"] = tc.sourceRepos
			if tc.sourceNamespaces != nil {
				project["sourceNamespaces"] = tc.sourceNamespaces
			}
			want := argopolicy.Authorize(project, repository, snapshot.Bindings[0].Destination, tc.namespace, snapshot.ArgoCDNamespace)
			if (want == nil) != tc.wantRender {
				t.Fatalf("argopolicy decision = %v", want)
			}
			rendered := false
			stop := errors.New("authorized render reached repo-server")
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot,
				invoke: func(context.Context, map[string]any) (Response, error) {
					invokeMu.Lock()
					defer invokeMu.Unlock()
					rendered = true
					return Response{}, stop
				}}
			_, err := client.RenderDesired(context.Background(), app, commit)
			if rendered != tc.wantRender || (!rendered && issueCode(err) != issueCode(want)) {
				t.Fatalf("repo-server rendered=%t err=%v, local Helm decision %v", rendered, err, want)
			}
		})
	}
}
