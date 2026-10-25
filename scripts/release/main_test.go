package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVersionInput(t *testing.T) {
	for _, tc := range []struct {
		version string
		valid   bool
	}{
		{"v0.1.0", true}, {"v1.2.3-rc.1", true}, {"v1.2.3-beta-test.0", true},
		{"v01.2.3", false}, {"v1.2.3-01", false}, {"1.2.3", false}, {"v1.2.3+build", false},
		{"v1.2.3;publish", false}, {"../../v1.2.3", false}, {"v1.2.3\n", false},
	} {
		if validVersion(tc.version) != tc.valid {
			t.Fatalf("version validation wrong for %q", tc.version)
		}
	}
}

func TestVersionedNotesPreserveReviewedContentAndProvenance(t *testing.T) {
	dir := t.TempDir()
	version, commit := "v0.1.0-rc.1", strings.Repeat("a", 40)
	platforms := []string{"darwin/arm64", "linux/amd64"}
	fallback, err := notesForRelease(dir, version, commit, platforms)
	if err != nil || fallback != releaseNotes(version, commit, platforms) {
		t.Fatal("absent notes changed the generic release contract")
	}
	folder := filepath.Join(dir, "docs", "release-notes")
	if err := os.MkdirAll(folder, 0755); err != nil {
		t.Fatal(err)
	}
	text := "# Pilot release\n\n신규 Application 내부 target과 미분석 경계를 검토한다.\n"
	if err := os.WriteFile(filepath.Join(folder, version+".md"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{commit, commit + "-dirty"} {
		notes, err := notesForRelease(dir, version, revision, platforms)
		if err != nil || !strings.Contains(notes, strings.TrimSpace(text)) || !strings.Contains(notes, "Source commit: `"+revision+"`") || !strings.Contains(notes, "darwin/arm64, linux/amd64") {
			t.Fatal("reviewed notes lost content or source/platform provenance")
		}
		if strings.Contains(notes, "modified local preview") != strings.HasSuffix(revision, "-dirty") {
			t.Fatal("notes misrepresented preview provenance")
		}
	}
}

func TestInvalidVersionedNotesCannotFallBackToSuccessfulRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil}, {"whitespace", []byte(" \n\t")}, {"too-large", bytes.Repeat([]byte("a"), (64<<10)+1)},
		{"invalid-utf8", []byte{0xff}}, {"nul", []byte("notes\x00")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			folder := filepath.Join(dir, "docs", "release-notes")
			if err := os.MkdirAll(folder, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(folder, "v0.1.0-rc.1.md"), tc.data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := notesForRelease(dir, "v0.1.0-rc.1", "fixture", []string{"linux/amd64"}); err == nil {
				t.Fatal("invalid maintainer notes were accepted")
			}
		})
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs", "release-notes", "v0.1.0-rc.1.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := notesForRelease(dir, "v0.1.0-rc.1", "fixture", nil); err == nil {
		t.Fatal("a directory was accepted as release notes")
	}
	if _, err := notesForRelease(dir, "../../outside", "fixture", nil); err == nil {
		t.Fatal("invalid version traversed the source notes directory")
	}
}

func TestArchivesPreserveContentPermissionsAndOrdering(t *testing.T) {
	files := []archiveFile{{"diffaro", []byte("fixture-executable"), 0755}, {"LICENSE", []byte("fixture-license"), 0644}}
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.tar.gz"), filepath.Join(dir, "second.tar.gz")
	if err := writeArchive(first, files); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(second, files); err != nil {
		t.Fatal(err)
	}
	left, err := checksum(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := checksum(second)
	if err != nil || left != right {
		t.Fatal("identical inputs produced different archives")
	}
	file, err := os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for _, expected := range files {
		entry, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil || entry.Name != expected.name || entry.Mode != expected.mode || !bytes.Equal(data, expected.data) {
			t.Fatal("archive changed file content, order or permissions", err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatal("unexpected archive member", err)
	}
	if err := writeArchive(first, files); err == nil {
		t.Fatal("existing archive was overwritten")
	}
}

func TestUnsupportedArchiveFormatCreatesNoOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsupported.zip")
	if err := writeArchive(path, nil); err == nil {
		t.Fatal("unsupported archive format accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected format created output")
	}
}

func TestExistingOutputIsPreserved(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "existing")
	if err := os.WriteFile(file, []byte("existing-release"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run("v0.1.0", file, "unused-go", false, targets); err == nil {
		t.Fatal("existing destination was accepted")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "existing-release" {
		t.Fatal("existing release changed during preflight")
	}
}

func TestUncommittedSourcesCannotProduceRelease(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is needed to test release source provenance")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture Git failed: %v %s", err, output)
		}
	}
	if err := run("v0.1.0", filepath.Join(dir, "dist"), "unused-go", false, targets); err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatal("untagged HEAD was accepted as a release")
	}
	if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run("v0.1.0", filepath.Join(dir, "dist"), "unused-go", false, targets); err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatal("uncommitted source was accepted as a release")
	}
	if _, err := os.Stat(filepath.Join(dir, "dist")); !os.IsNotExist(err) {
		t.Fatal("rejected release left an output directory")
	}
}

func TestTargetSelectionIsExplicitAndStable(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []target
	}{
		{"all", []target{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}},
		{"darwin/arm64", []target{{"darwin", "arm64"}}},
		{"darwin/arm64,linux/amd64", []target{{"linux", "amd64"}, {"darwin", "arm64"}}},
	} {
		got, err := selectTargets(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("wrong release platforms for %q: %v %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"windows/amd64", "windows/arm64", "", "darwin", "darwin/arm64,darwin/arm64", "darwin/arm64,", "darwin/arm64,unknown/amd64", "all,darwin/arm64", "../darwin/arm64"} {
		if _, err := selectTargets(input); err == nil {
			t.Fatalf("invalid platform selection accepted: %q", input)
		}
	}
}

func TestNestedExportCannotPackageParentRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is needed to check release provenance")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	if output, err := exec.Command("git", "init").CombinedOutput(); err != nil {
		t.Fatalf("fixture Git failed: %v %s", err, output)
	}
	nested := filepath.Join(dir, "public")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	output := filepath.Join(nested, "dist")
	err := run("v0.1.0-preview.1", output, "unused-go", true, targets)
	if err == nil || !strings.Contains(err.Error(), "independent release repository") {
		t.Fatal("nested export used parent source provenance", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("rejected export left an output directory")
	}
}
