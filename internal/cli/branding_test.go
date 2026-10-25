package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestDiffaroFirstRunDefaults(t *testing.T) {
	git := gitServer(t)
	backend, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	t.Setenv("DIFFARO_ARGOCD_TOKEN", "fictional-argo-0")
	t.Setenv("DIFFARO_GITHUB_TOKEN", "fictional-github-token")
	t.Chdir(t.TempDir())
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, &out, &diagnostic); code != 0 || !strings.Contains(out.String(), "Usage: diffaro") || strings.Contains(out.String(), "kubeseer") {
		t.Fatal("help does not describe the installed command")
	}
	out.Reset()
	if code := Run(context.Background(), []string{"init", "--endpoint", backend.URL, "--application", "argocd/api", "--git-api", git.URL}, &out, &diagnostic); code != 0 {
		t.Fatalf("default init failed: %d %s", code, diagnostic.String())
	}
	data, err := os.ReadFile("diffaro.local.yaml")
	if err != nil || !bytes.Contains(data, []byte("DIFFARO_ARGOCD_TOKEN")) || !bytes.Contains(data, []byte("DIFFARO_GITHUB_TOKEN")) || !bytes.Contains(data, []byte("fictional-argo-0")) {
		t.Fatal("new profile lost branded token references or failed to save the token")
	}
	if err := os.WriteFile("diffaro.yaml", data, 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run(context.Background(), []string{"validate"}, &out, &diagnostic); code != 0 {
		t.Fatalf("default profile lookup failed: %d %s", code, diagnostic.String())
	}
	legacy := bytes.ReplaceAll(data, []byte("DIFFARO_ARGOCD_TOKEN"), []byte("KUBESEER_ARGOCD_TOKEN"))
	if err := os.WriteFile("kubeseer.local.yaml", legacy, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DIFFARO_ARGOCD_TOKEN", "")
	t.Setenv("KUBESEER_ARGOCD_TOKEN", "fictional-argo-0")
	if code := Run(context.Background(), []string{"doctor", "--config", "kubeseer.local.yaml"}, &out, &diagnostic); code != 0 {
		t.Fatalf("explicit legacy profile/token stopped working: %d %s", code, diagnostic.String())
	}
}
