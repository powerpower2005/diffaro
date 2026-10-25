// Package main builds release archives without requiring a packaging service.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type target struct{ os, arch string }

var targets = []target{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func main() {
	version := flag.String("version", "", "release tag, such as v0.1.0 or v0.1.0-rc.1")
	output := flag.String("output", "dist", "new destination directory; existing paths are never replaced")
	goTool := flag.String("go", "go", "Go compiler executable")
	platforms := flag.String("targets", "all", "all or comma-separated release targets, such as darwin/arm64")
	preview := flag.Bool("preview", false, "local preview from untagged or modified sources; always marked modified")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	selected, err := selectTargets(*platforms)
	if err == nil {
		err = run(*version, *output, *goTool, *preview, selected)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func selectTargets(value string) ([]target, error) {
	if value == "all" {
		return append([]target{}, targets...), nil
	}
	requested := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" || requested[item] {
			return nil, fmt.Errorf("targets must contain unique supported OS/architecture pairs")
		}
		requested[item] = true
	}
	selected := []target{}
	for _, platform := range targets {
		key := platform.os + "/" + platform.arch
		if requested[key] {
			selected = append(selected, platform)
			delete(requested, key)
		}
	}
	if len(requested) != 0 || len(selected) == 0 {
		return nil, fmt.Errorf("unsupported release target; use linux or darwin with amd64 or arm64")
	}
	return selected, nil
}

func validVersion(version string) bool {
	if !tagPattern.MatchString(version) {
		return false
	}
	_, pre, found := strings.Cut(version, "-")
	if found {
		for _, part := range strings.Split(pre, ".") {
			if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
				return false
			}
		}
	}
	return true
}

func git(args ...string) (string, error) {
	data, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("cannot read release Git metadata; run from this repository with Git installed")
	}
	return strings.TrimSpace(string(data)), nil
}

func run(version, destination, goTool string, preview bool, selected []target) error {
	if !validVersion(version) {
		return fmt.Errorf("version must be vMAJOR.MINOR.PATCH with an optional SemVer prerelease")
	}
	output, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("invalid output directory")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return fmt.Errorf("output must be a new directory; existing paths are not replaced")
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	rootInfo, rootErr := os.Stat(root)
	workingInfo, workingErr := os.Stat(".")
	if rootErr != nil || workingErr != nil || !os.SameFile(rootInfo, workingInfo) {
		return fmt.Errorf("run packaging from the root of the independent release repository")
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil || !commitPattern.MatchString(commit) {
		return fmt.Errorf("cannot resolve the release commit")
	}
	status, err := git("status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if !preview {
		if status != "" {
			return fmt.Errorf("release requires a clean checkout; use --preview only for local checks")
		}
		tagCommit, err := git("rev-parse", "--verify", "refs/tags/"+version+"^{commit}")
		if err != nil || tagCommit != commit {
			return fmt.Errorf("release tag must exist and point to HEAD; use --preview for untagged local checks")
		}
	} else {
		commit += "-dirty"
	}
	if strings.ContainsAny(goTool, `/\`) {
		goTool, err = filepath.Abs(goTool)
		if err != nil {
			return fmt.Errorf("invalid Go executable path")
		}
	}
	platformNames := []string{}
	for _, platform := range selected {
		platformNames = append(platformNames, platform.os+"/"+platform.arch)
	}
	notes, err := notesForRelease(root, version, commit, platformNames)
	if err != nil {
		return err
	}
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return fmt.Errorf("release requires the repository LICENSE")
	}
	dependencyNotices, err := dependencyLicenses(goTool, root)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".diffaro-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	var checksums strings.Builder
	for _, platform := range selected {
		binaryName := "diffaro"
		binaryPath := filepath.Join(stage, binaryName)
		linker := "-s -w -X diffaro/internal/cli.releaseVersion=" + version + " -X diffaro/internal/cli.releaseRevision=" + commit + " -X diffaro/internal/httpx.userAgentVersion=" + version
		cmd := exec.Command(goTool, "build", "-trimpath", "-buildvcs=false", "-ldflags", linker, "-o", binaryPath, "./cmd/diffaro")
		cmd.Dir = root
		cmd.Env = buildEnvironment(platform)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		fmt.Printf("Building %s/%s\n", platform.os, platform.arch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build failed for %s/%s", platform.os, platform.arch)
		}
		if platform.os == runtime.GOOS && platform.arch == runtime.GOARCH {
			smoke := exec.Command(binaryPath, "--version")
			data, err := smoke.Output()
			if err != nil || !strings.HasPrefix(string(data), "Diffaro "+version) || !strings.Contains(string(data), commit[:12]) {
				return fmt.Errorf("native release binary failed its version check")
			}
		}
		binary, err := os.ReadFile(binaryPath)
		if err != nil {
			return err
		}
		readme := fmt.Sprintf("Diffaro %s\nCommit: %s\nPlatform: %s/%s\n\nExtract and run diffaro --help.\nGo and Git are not required to run the binary.\nUse init --local-helm with read-only GitHub and Argo CD access for supported new child analysis. The engine is bundled; --helm-binary PATH selects a trusted native Helm executable. No repo-server, kubeconfig or helper service is needed for local Helm.\nUse --format terminal --json-output preview.json for readable output and the full report.\nOptional repo-server rendering requires grpcurl; automatic Kubernetes context and forwarding require kubectl and a trusted kubeconfig.\n", version, commit, platform.os, platform.arch)
		files := []archiveFile{{binaryName, binary, 0755}, {"LICENSE", license, 0644}, {"README.txt", []byte(readme), 0644}}
		files = append(files, dependencyNotices...)
		name := "diffaro_" + version + "_" + platform.os + "_" + platform.arch
		name += ".tar.gz"
		archivePath := filepath.Join(stage, name)
		if err := writeArchive(archivePath, files); err != nil {
			return err
		}
		if err := os.Remove(binaryPath); err != nil {
			return err
		}
		hash, err := checksum(archivePath)
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%s  %s\n", hash, name)
	}
	if err := os.WriteFile(filepath.Join(stage, "SHA256SUMS"), []byte(checksums.String()), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "RELEASE_NOTES.md"), []byte(notes), 0644); err != nil {
		return err
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return fmt.Errorf("output appeared during packaging; nothing was replaced")
	}
	if err := os.Rename(stage, output); err != nil {
		return err
	}
	fmt.Printf("Release packages saved: %s\n", output)
	return nil
}

func releaseNotes(version, commit string, platforms []string) string {
	return fmt.Sprintf("# Diffaro %s\n\nRead-only target-to-target previews for GitHub, Argo CD and Git Helm.\n\n- Platforms: %s. Verify the selected archive against SHA256SUMS before extraction.\n- Go and Git are not required at runtime. Profiles and reports use v1alpha1 schemas.\n- init --local-helm uses bundled Helm with authenticated API context; supported new or reconfigured child resources can be analyzed without repo-server, kubeconfig or a helper service.\n- --helm-binary PATH selects a trusted Helm 3/4 native executable; custom server equivalence remains unverified.\n- --format terminal --json-output preview.json shows readable changes and saves the full report.\n- Reports show target changes; operational risk is judged by reviewers. Failed/skipped branches remain explicit.\n- Optional repo-server analysis uses grpcurl; automatic context/tunnels also require kubectl and a trusted kubeconfig.\n- Cross-build and mock installation checks do not certify every native architecture or organization backend.\n\nSource commit: `%s`\n", version, strings.Join(platforms, ", "), commit)
}

// Versioned maintainer notes travel with the source. Runtime PR content is
// never consulted, and invalid notes cannot silently fall back to generic text.
func notesForRelease(root, version, commit string, platforms []string) (string, error) {
	if !validVersion(version) {
		return "", fmt.Errorf("invalid release notes version")
	}
	path := filepath.Join(root, "docs", "release-notes", version+".md")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return releaseNotes(version, commit, platforms), nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return "", fmt.Errorf("versioned release notes must be a regular nonempty file of at most 64 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read versioned release notes")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" || strings.ContainsRune(string(data), '\x00') {
		return "", fmt.Errorf("versioned release notes must contain bounded nonempty UTF-8 text without NUL")
	}
	notes := strings.TrimSpace(string(data)) + fmt.Sprintf("\n\n---\n\nPackaged platforms: %s. Verify archives against `SHA256SUMS`.\n\nSource commit: `%s`\n", strings.Join(platforms, ", "), commit)
	if strings.HasSuffix(commit, "-dirty") {
		notes += "\nThis is a modified local preview, not an official release.\n"
	}
	return notes, nil
}

func buildEnvironment(platform target) []string {
	env := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case "GOOS", "GOARCH", "CGO_ENABLED", "GOAMD64", "GOARM64", "GOFLAGS":
			continue
		}
		env = append(env, value)
	}
	return append(env, "GOOS="+platform.os, "GOARCH="+platform.arch, "CGO_ENABLED=0", "GOAMD64=v1", "GOARM64=v8.0")
}

func dependencyLicenses(goTool, root string) ([]archiveFile, error) {
	goRoot, err := exec.Command(goTool, "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("cannot locate Go's license")
	}
	goLicense, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(goRoot)), "LICENSE"))
	if err != nil {
		return nil, fmt.Errorf("cannot include Go's license")
	}
	files := []archiveFile{{"licenses/go-LICENSE.txt", goLicense, 0644}}
	cmd := exec.Command(goTool, "list", "-deps", "-f", `{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}{{end}}`, "./cmd/diffaro")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cannot collect binary dependency licenses; prepare the Go module cache")
	}
	modules := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 {
			continue
		}
		modules[parts[0]+"@"+parts[1]] = parts[2]
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		notices, err := moduleNotices(name, modules[name])
		if err != nil {
			return nil, err
		}
		files = append(files, notices...)
	}
	return files, nil
}

// Preserve every root license and notice, including dual-license alternatives.
// Do not choose one alternative or translate the upstream text.
func moduleNotices(module, directory string) ([]archiveFile, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("cannot read dependency notices for %s", module)
	}
	files := []archiveFile{}
	hasLicense := false
	for _, entry := range entries {
		name := strings.ToUpper(entry.Name())
		license := noticeName(name, "LICENSE") || noticeName(name, "LICENCE") || noticeName(name, "COPYING")
		if !license && !noticeName(name, "NOTICE") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 2<<20 {
			return nil, fmt.Errorf("dependency notice must be a nonempty regular file within 2 MiB: %s/%s", module, entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("cannot include dependency notice %s/%s", module, entry.Name())
		}
		archiveName := "licenses/" + strings.ReplaceAll(module, "/", "_") + "-" + entry.Name()
		files = append(files, archiveFile{archiveName, data, 0644})
		hasLicense = hasLicense || license
	}
	if !hasLicense {
		return nil, fmt.Errorf("dependency license missing for %s; expected LICENSE, LICENCE or COPYING with optional suffix", module)
	}
	return files, nil
}

func noticeName(name, prefix string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".") || strings.HasPrefix(name, prefix+"-")
}

type archiveFile struct {
	name string
	data []byte
	mode int64
}

func writeArchive(path string, files []archiveFile) (err error) {
	if !strings.HasSuffix(path, ".tar.gz") {
		return fmt.Errorf("release archives must use .tar.gz")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { err = firstError(err, out.Close()) }()
	zipped := gzip.NewWriter(out)
	writer := tar.NewWriter(zipped)
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), ModTime: time.Unix(0, 0)}); err != nil {
			return firstError(err, writer.Close(), zipped.Close())
		}
		if _, err := writer.Write(file.data); err != nil {
			return firstError(err, writer.Close(), zipped.Close())
		}
	}
	return firstError(writer.Close(), zipped.Close())
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func checksum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
