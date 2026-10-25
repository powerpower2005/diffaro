package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func bundleText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return all.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString("== " + header.Name + "\n")
		io.Copy(&all, tr)
	}
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("%s: %v mode %v", path, err, info)
	}
}

// Secret canaries in tokens, Application values and backend error bodies
// reach none of the report, trace, bundle (either level), names file or
// stderr; the public bundle also names no host, repository or Application.
func TestDebugBundleCarriesNoSecretsAndPublicCarriesNoNames(t *testing.T) {
	for _, level := range []string{"public", "private"} {
		t.Run(level, func(t *testing.T) {
			git := gitServer(t)
			argo, _ := argoServer(t, "fictional-argo-0", backendOptions{headFailure: true, secret: true})
			config := configFile(t, git.URL, argo.URL)
			dir := t.TempDir()
			bundle := filepath.Join(dir, "bundle.tar.gz")
			var stdout, stderr bytes.Buffer
			args := []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "json", "--output", filepath.Join(dir, "report.json"), "--debug-bundle", bundle, "--debug-bundle-level", level, "--quiet"}
			if level == "private" {
				// The public run has no --trace: the bundle collects its own.
				args = append(args, "--trace", filepath.Join(dir, "trace.json"))
			}
			code := Run(context.Background(), args, &stdout, &stderr)
			if code != domain.ExitFailed && code != domain.ExitPartialFailure {
				t.Fatalf("code = %d stderr=%s", code, stderr.String())
			}
			assertPrivateFile(t, bundle)
			text := bundleText(t, bundle)
			artifacts := text + stdout.String() + stderr.String()
			for _, name := range []string{"report.json", "trace.json"} {
				data, _ := os.ReadFile(filepath.Join(dir, name))
				artifacts += string(data)
			}
			names := debugNamesPath(bundle)
			if level == "public" {
				assertPrivateFile(t, names)
				data, _ := os.ReadFile(names)
				artifacts += string(data)
				if !strings.Contains(string(data), `"api"`) {
					t.Fatalf("names file lacks the Application name: %s", data)
				}
				for _, private := range []string{strings.TrimPrefix(argo.URL, "http://"), strings.TrimPrefix(git.URL, "http://"), "example/charts", `"api"`, "DIFFARO_TEST_"} {
					if strings.Contains(text, private) {
						t.Fatalf("public bundle names %q:\n%s", private, text)
					}
				}
				if !strings.Contains(text, `"steps": [`) || !strings.Contains(text, `"code": "backend_error"`) || !strings.Contains(text, `"route": "/api/v1/applications/{application}/manifests"`) {
					t.Fatalf("public bundle lost the failure structure:\n%s", text)
				}
			} else {
				if _, err := os.Stat(names); !os.IsNotExist(err) {
					t.Fatalf("private bundle wrote a names file: %v", err)
				}
				if !strings.Contains(text, "== diffaro-debug-bundle/report.json") || !strings.Contains(text, "== diffaro-debug-bundle/trace.json") {
					t.Fatalf("private bundle lacks report or trace:\n%s", text)
				}
			}
			assertNoSecrets(t, level+" artifacts", artifacts, "fictional-argo-", "fictional-github-token", "fictional-sensitive-error", "fictional-inline-value", "fictional-object-value")
		})
	}
}

// The debug bundle options are rejected before any provider is contacted
// when the command or combination does not support them.
func TestDebugBundleOptionValidation(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle.tar.gz")
	for _, args := range [][]string{
		{"doctor", "--debug-bundle", bundle},
		{"validate", "--debug-bundle", bundle},
		{"diff", "--debug-bundle-level", "private"},
		{"diff", "--debug-bundle", bundle, "--debug-bundle-level", "everything"},
		{"diff", "--debug-bundle", bundle, "--output", bundle},
		{"diff", "--debug-bundle", bundle, "--trace", bundle + ".names.json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), append(args, "--config", filepath.Join(dir, "missing.yaml")), &stdout, &stderr); code != domain.ExitInvalidConfiguration || !strings.Contains(stderr.String(), "debug-bundle") {
			t.Fatalf("%v: code %d stderr %s", args, code, stderr.String())
		}
	}
}
