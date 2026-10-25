package cli

import (
	"context"
	"strings"
	"testing"

	"diffaro/internal/gitlocal"
	"diffaro/internal/profile"
)

// git.localRepositories put a local reader in front of the provider for every
// source read of the run: local Helm, its shared cache, changed-path
// selection and input changes. Without git the run states that the local
// repositories were not used.
func localRepositoriesProfile() profile.Config {
	return profile.Config{Version: "v1alpha1", Git: profile.Git{APIURL: "https://git.example.invalid/api/v3", Repository: "example/charts", RepositoryURL: "https://git.example.invalid/example/charts"},
		Instances: []profile.Instance{{ID: "east", Endpoint: "https://argocd.example.invalid"}}, Render: profile.DefaultRender()}
}

func TestLocalRepositoriesFrontTheSourceReads(t *testing.T) {
	c := localRepositoriesProfile()
	c.Git.LocalRepositories = []profile.LocalRepository{{Name: "example/charts", Path: "/fictional/mirrors/charts"}}
	c.Git.GitExecutable = "/fictional/bin/git"
	c.Instances[0].LocalHelm = true
	b := buildBackends(context.Background(), c, nil)
	local, ok := b.source.(*gitlocal.Reader)
	if !ok || local != b.local || local.Paths["example/charts"] != "/fictional/mirrors/charts" || local.Executable != "/fictional/bin/git" || local.API != b.git || len(b.notes) != 0 {
		t.Fatalf("source reader = %T %+v notes=%v", b.source, local, b.notes)
	}

	t.Setenv("PATH", "")
	c.Git.GitExecutable = ""
	b = buildBackends(context.Background(), c, nil)
	if b.local != nil || b.source != b.git || len(b.notes) != 1 || !strings.Contains(b.notes[0], "no git executable was found") {
		t.Fatalf("missing git: local=%v notes=%v", b.local, b.notes)
	}

	plain := localRepositoriesProfile()
	if b = buildBackends(context.Background(), plain, nil); b.local != nil || b.source != b.git {
		t.Fatal("a profile without local repositories must read through the provider")
	}
}
