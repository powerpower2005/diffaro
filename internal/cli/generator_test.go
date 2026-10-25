package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// A diff report and its trace name the build, the command, the profile
// shape and each instance's Argo CD version, without hosts,
// repositories, Application names or credentials.
func TestDiffReportAndTraceNameTheGeneratorAndServerVersion(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := configFile(t, git.URL, argo.URL)
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	report, code := runJSON(t, config, "--base", "main", "--head", "feature", "--trace", tracePath)
	if code != domain.ExitChangesFound {
		t.Fatalf("code = %d", code)
	}
	g := report.Generator
	if g == nil || g.Command != "diff" || g.GoVersion != runtime.Version() || g.Platform != runtime.GOOS+"/"+runtime.GOARCH || g.Version == "" || g.Revision == "" {
		t.Fatalf("generator = %+v", g)
	}
	if g.Profile.Instances != 1 || !slices.Equal(g.Profile.Backends, []string{"argocd-application-api"}) {
		t.Fatalf("profile shape = %+v", g.Profile)
	}
	if len(report.RenderBackends) != 1 || report.RenderBackends[0].Version != "v3.5.3" {
		t.Fatalf("render backends = %+v", report.RenderBackends)
	}
	encoded, _ := json.Marshal(g)
	host := strings.TrimPrefix(argo.URL, "http://")
	for _, private := range []string{host, strings.TrimPrefix(git.URL, "http://"), "example/charts", `"api"`, "fictional-"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("generator names %q: %s", private, encoded)
		}
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		Generator *domain.Generator `json:"generator"`
	}
	if err := json.Unmarshal(data, &trace); err != nil || trace.Generator == nil || trace.Generator.Revision != g.Revision || trace.Generator.Command != "diff" {
		t.Fatalf("trace generator = %+v (%v)", trace.Generator, err)
	}
}

// The version read is evidence only: when it fails, the status, exit code
// and comparison stay those of a run whose read succeeds, and the report
// states the missing version as a limitation.
func TestVersionLookupFailureNeverChangesTheResult(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	reference, referenceCode := runJSON(t, configFile(t, git.URL, argo.URL), "--base", "main", "--head", "feature")
	failing := overlayArgoCD(t, argo, map[string]http.HandlerFunc{"/api/version": func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fictional-sensitive-error", http.StatusInternalServerError)
	}})
	report, code := runJSON(t, configFile(t, git.URL, failing.URL), "--base", "main", "--head", "feature")
	if code != referenceCode || report.Status != reference.Status || len(report.Issues) != len(reference.Issues) || report.ChangesFound != reference.ChangesFound {
		t.Fatalf("version failure changed the result: code %d→%d status %s→%s issues %d→%d", referenceCode, code, reference.Status, report.Status, len(reference.Issues), len(report.Issues))
	}
	if report.RenderBackends[0].Version != "" || report.RenderBackends[0].Compatibility != reference.RenderBackends[0].Compatibility {
		t.Fatalf("render backend = %+v", report.RenderBackends[0])
	}
	if !slices.ContainsFunc(report.Limitations, func(l string) bool {
		return strings.Contains(l, "instance-0 version could not be read (backend_error)")
	}) {
		t.Fatalf("limitations = %v", report.Limitations)
	}
}

// Demo reports come from embedded fixtures and name no generator, so the
// published examples stay identical across builds.
func TestDemoReportNamesNoGenerator(t *testing.T) {
	var out, errout bytes.Buffer
	Run(context.Background(), []string{"demo", "--format", "json"}, &out, &errout)
	if !json.Valid(out.Bytes()) || bytes.Contains(out.Bytes(), []byte(`"generator"`)) {
		t.Fatalf("demo output: %s", out.String())
	}
}
