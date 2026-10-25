package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionsRejectUnsafeOrUnboundedDocuments(t *testing.T) {
	for _, data := range []string{
		"", "instances: []", "instances: [\n", "instances: []\n---\ninstances: []", "instances: []\ninstances: []",
		"instances:\n  - id: dev\n    endpoint: https://argocd.example.com\n    token: 'fictional-sensitive-error invalid'\n", strings.Repeat(" ", MaxConfigBytes+1),
	} {
		file := filepath.Join(t.TempDir(), "connections.yaml")
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConnections(file); err == nil || strings.Contains(err.Error(), "fictional-") {
			t.Fatal("invalid catalog accepted or credential echoed")
		}
	}
}

func TestConnectionsResolvePathsRelativeToCatalog(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "connections.yaml")
	data := `instances:
  - id: dev
    endpoint: https://argocd.example.com
    tokenEnv: DEV_TOKEN
    repoServer:
      address: repo-server.example.com:8081
      grpcurl: bin/grpcurl
      contextFile: context.json
      caFile: ca.pem
`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadConnections(file)
	if err != nil {
		t.Fatal(err)
	}
	rpc := catalog.Instances[0].RepoServer
	if rpc.GRPCURL != filepath.Join(dir, "bin", "grpcurl") || rpc.ContextFile != filepath.Join(dir, "context.json") || rpc.CAFile != filepath.Join(dir, "ca.pem") {
		t.Fatal("relocated output profile would use different render context files")
	}
}
