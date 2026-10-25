package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModuleNoticesKeepAlternateNamesAndDualLicenses(t *testing.T) {
	for _, names := range [][]string{{"LICENSE"}, {"LICENSE.txt"}, {"license.md"}, {"COPYING"}, {"COPYING.md", "LICENSE.BSD", "LICENSE.MPL-2.0", "NOTICE"}, {"LICENCE", "NOTICE.txt"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range names {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("upstream text "+name), 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Files with similar words are not legal notices.
			_ = os.WriteFile(filepath.Join(dir, "LICENSE_test.go"), []byte("not a license"), 0644)
			got, err := moduleNotices("example.invalid/library@v1.0.0", dir)
			if err != nil || len(got) != len(names) {
				t.Fatal(err, got)
			}
			seen := map[string]string{}
			for _, f := range got {
				seen[strings.TrimPrefix(f.name, "licenses/example.invalid_library@v1.0.0-")] = string(f.data)
				if f.mode != 0644 {
					t.Fatal("notice permissions changed")
				}
			}
			for _, name := range names {
				if seen[name] != "upstream text "+name {
					t.Fatal("notice content or alternative lost", seen)
				}
			}
			repeat, err := moduleNotices("example.invalid/library@v1.0.0", dir)
			if err != nil || !reflect.DeepEqual(got, repeat) {
				t.Fatal("notice collection is nondeterministic")
			}
		})
	}
}

func TestModuleNoticesMissingOrInvalidLicenseFails(t *testing.T) {
	for _, mode := range []string{"missing", "notice-only", "empty", "directory", "too-large"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			switch mode {
			case "notice-only":
				_ = os.WriteFile(filepath.Join(dir, "NOTICE"), []byte("notice"), 0644)
			case "empty":
				_ = os.WriteFile(filepath.Join(dir, "LICENSE"), nil, 0644)
			case "directory":
				_ = os.Mkdir(filepath.Join(dir, "LICENSE"), 0755)
			case "too-large":
				_ = os.WriteFile(filepath.Join(dir, "LICENSE"), make([]byte, (2<<20)+1), 0644)
			}
			if _, err := moduleNotices("example.invalid/library@v1.0.0", dir); err == nil {
				t.Fatal("unlicensed or invalid dependency silently accepted")
			}
		})
	}
}

func TestActualDependencyNotices(t *testing.T) {
	tool := os.Getenv("DIFFARO_TEST_GO_BINARY")
	if tool == "" {
		t.Skip("set DIFFARO_TEST_GO_BINARY to verify the actual dependency cache")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	files, err := dependencyLicenses(tool, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go-LICENSE.txt", "github.com_BurntSushi_toml@", "github.com_Masterminds_sprig/v3@", "LICENSE.BSD", "LICENSE.MPL-2.0"} {
		found := false
		for _, f := range files {
			found = found || strings.Contains(f.name, strings.ReplaceAll(name, "/", "_"))
		}
		if !found {
			t.Fatal("missing dependency notice", name)
		}
	}
}

func TestReleaseNotesDescribeCurrentAnalysis(t *testing.T) {
	notes := releaseNotes("v0.1.0-rc.1", strings.Repeat("a", 40), []string{"linux/amd64", "darwin/arm64"})
	for _, want := range []string{"SHA256SUMS", "--local-helm", "--helm-binary PATH", "--format terminal", "v1alpha1", "operational risk is judged by reviewers", "grpcurl", "trusted kubeconfig", "linux/amd64", "darwin/arm64", strings.Repeat("a", 40)} {
		if !strings.Contains(notes, want) {
			t.Fatal("release notes omit the current contract", want)
		}
	}
}
