package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/profile"
)

// init --pr names Applications that read the PR repository through a host
// the new profile does not declare, and saves the render timeout,
// concurrency and external Helm environment it is given.
func TestInitNamesUndeclaredMirrorsAndSavesRenderSettings(t *testing.T) {
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	mirrored := testApp()
	mirrored["metadata"] = map[string]any{"name": "mirrored", "namespace": "argocd"}
	mirrored["spec"].(map[string]any)["source"].(map[string]any)["repoURL"] = "https://git-mirror.example.com/example/charts"
	server := overlayArgoCD(t, a, map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) { writeInventory(w, testApp(), mirrored) }})
	helm := filepath.Join(t.TempDir(), "helm")
	if err := os.WriteFile(helm, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--single-instance", "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}
	run := func(extra ...string) (int, string, string) {
		path := filepath.Join(t.TempDir(), "profile.yaml")
		var out, diagnostic bytes.Buffer
		code := Run(context.Background(), append(append(append([]string{}, base...), extra...), "--config", path), &out, &diagnostic)
		data, _ := os.ReadFile(path)
		return code, out.String() + diagnostic.String(), string(data)
	}

	code, output, data := run("--helm-binary", helm, "--timeout", "45s", "--max-concurrency", "6", "--helm-env", "LOOKUP_HOST=https://lookup.example.invalid", "--helm-env-from", "LOOKUP_TOKEN=FICTIONAL_LOOKUP_TOKEN")
	if code != 0 {
		t.Fatalf("init failed: %d %s", code, output)
	}
	if !strings.Contains(output, "Render limits: 45s per stage; 6 concurrent renders;") {
		t.Fatalf("init output does not state the saved render settings:\n%s", output)
	}
	// One Application uses the repository directly, so the profile is
	// written and the mirrored one is a warning.
	if !strings.Contains(output, "Warning: 1 Application(s) read the PR repository from https://git-mirror.example.com/example/charts and were not selected") {
		t.Fatalf("undeclared mirror not named:\n%s", output)
	}
	c, err := profile.Parse([]byte(data))
	if err != nil {
		t.Fatalf("saved profile does not load: %v\n%s", err, data)
	}
	env := c.Instances[0].HelmEnvironment
	if c.Render.Timeout != "45s" || c.Render.MaxConcurrency != 6 || len(env) != 2 || env[0] != (profile.HelmEnvVar{Name: "LOOKUP_HOST", Value: "https://lookup.example.invalid"}) || env[1] != (profile.HelmEnvVar{Name: "LOOKUP_TOKEN", ValueFromEnv: "FICTIONAL_LOOKUP_TOKEN"}) {
		t.Fatalf("render settings or Helm environment not saved: %+v %+v", c.Render, env)
	}

	// Declaring the mirror drops the hint.
	if code, output, _ = run("--mirror-host", "https://git-mirror.example.com"); code != 0 || strings.Contains(output, "were not selected") {
		t.Fatalf("hint shown for a declared mirror: %d\n%s", code, output)
	}
	// Only an external Helm executable receives environment variables, and
	// the profile rules apply to the names.
	for _, extra := range [][]string{{"--helm-env", "LOOKUP_HOST=x"}, {"--helm-binary", helm, "--helm-env", "LOOKUP_HOST"}, {"--helm-binary", helm, "--helm-env", "HOME=/tmp"}, {"--timeout", "2h"}, {"--max-concurrency", "0"}} {
		if code, output, data := run(extra...); code != 4 || data != "" {
			t.Fatalf("%v: exit %d, profile written=%t\n%s", extra, code, data != "", output)
		}
	}
}
