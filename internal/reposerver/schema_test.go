package reposerver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"diffaro/internal/profile"
)

func schemaFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "diffaro-repo-server-*.proto"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A Client writes the embedded wire schema once for all of its RPCs and
// removes it on Close; an invoker without a Client schema removes its file
// after each RPC.
func TestWireSchemaIsWrittenOncePerClientAndRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := profile.RepoServer{GRPCURL: executable, Address: "127.0.0.1:8081", Plaintext: true}
	if _, err := processMethodInvoker(config, 1024, Method, nil)(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if files := schemaFiles(t, dir); len(files) != 0 {
		t.Fatalf("per-RPC schema file was not removed: %v", files)
	}
	client := &Client{schema: &protoSchema{}}
	render := processMethodInvoker(config, 1024, Method, client.schema)
	for range 3 {
		if _, err := render(context.Background(), map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	files := schemaFiles(t, dir)
	if len(files) != 1 || files[0] != client.schema.path {
		t.Fatalf("Client RPCs must share one schema file: %v", files)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if files := schemaFiles(t, dir); len(files) != 0 {
		t.Fatalf("Close did not remove the schema file: %v", files)
	}
}
