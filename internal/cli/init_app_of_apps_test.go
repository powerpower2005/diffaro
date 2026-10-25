package cli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// An API-only init --pr warns when Applications using the PR repository
// manage child Applications, whose branch-tracking or changed children that
// backend cannot analyze; local Helm and leaf Applications get no warning.
func TestInitWarnsAPIOnlyProfilesAboutAppOfApps(t *testing.T) {
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	parent := testApp()
	parent["status"] = map[string]any{"resources": []any{map[string]any{"group": "argoproj.io", "kind": "Application", "namespace": "argocd", "name": "child"}}}
	leaf := testApp()
	leaf["metadata"] = map[string]any{"name": "leaf", "namespace": "argocd"}
	leaf["status"] = map[string]any{"resources": []any{map[string]any{"group": "apps", "kind": "Deployment", "namespace": "demo", "name": "api"}}}
	other := testApp()
	other["metadata"] = map[string]any{"name": "other-parent", "namespace": "argocd"}
	other["spec"].(map[string]any)["source"].(map[string]any)["repoURL"] = "https://github.com/example/other"
	other["status"] = parent["status"]
	for _, tc := range []struct {
		name  string
		items []any
		extra []string
		warn  bool
	}{
		{"api-only parent", []any{parent, leaf, other}, nil, true},
		{"local Helm parent", []any{parent, leaf, other}, []string{"--local-helm"}, false},
		{"leaves and other repositories only", []any{leaf, other}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := tc.items
			server := overlayArgoCD(t, a, map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) { writeInventory(w, items...) }})
			args := append([]string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--single-instance", "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB", "--config", filepath.Join(t.TempDir(), "profile.yaml")}, tc.extra...)
			var out, diagnostic bytes.Buffer
			if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
				t.Fatalf("init failed: %d %s", code, diagnostic.String())
			}
			warned := strings.Contains(diagnostic.String(), "Warning: 1 Application(s) using the PR repository manage child Applications") && strings.Contains(diagnostic.String(), "--local-helm")
			if warned != tc.warn {
				t.Fatalf("warning shown=%t, want %t\n%s", warned, tc.warn, diagnostic.String())
			}
		})
	}
}
