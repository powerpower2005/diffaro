package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/profile"
)

func TestLocalHelmCatalogInitRequiresNoExternalTools(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "connections.yaml")
	if err := os.WriteFile(catalog, []byte("instances:\n  - id: root\n    endpoint: https://argo.example.invalid\n    tokenEnv: TEST_ARGO_TOKEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "profile.yaml")
	t.Setenv("PATH", "")
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"init", "--connections", catalog, "--pr", "https://github.com/example/charts/pull/12", "--local-children", "--local-helm", "--config", config}, &out, &diagnostic)
	if code != 0 || !strings.Contains(out.String(), "bundled local Helm") {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	c, err := profile.Load(config)
	if err != nil || !c.Instances[0].LocalHelm || c.Instances[0].RepoServer != nil {
		t.Fatal("local backend was not saved independently", err)
	}
	if code = Run(context.Background(), []string{"validate", "--config", config}, &out, &diagnostic); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	code = Run(context.Background(), []string{"init", "--endpoint", "https://argo.example.invalid", "--local-helm", "--repo-server", "127.0.0.1:8081", "--config", filepath.Join(dir, "conflict.yaml")}, &out, &diagnostic)
	if code != 4 {
		t.Fatal("competing backends accepted", code)
	}
}
