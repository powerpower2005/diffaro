package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"diffaro/internal/domain"
)

// Scenario runs exercise success and partial failure through mock providers.
func TestScenarioReports(t *testing.T) {
	cases := []struct {
		name      string
		config    func(*testing.T) string
		recursive bool
		exit      int
	}{
		{"basic-child-skipped", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := recursiveServer(t, "new", "fictional-argo-0", false, false)
			return recursiveConfig(t, g.URL, a.URL)
		}, true, 1},
		{"recursive-change", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := recursiveServer(t, "normal", "fictional-argo-0", false, false)
			return recursiveConfig(t, g.URL, a.URL)
		}, true, 1},
		{"recursive-add", func(t *testing.T) string { return mockConfig(t, true, true) }, true, 1},
		{"recursive-remove", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := recursiveServer(t, "removed", "fictional-argo-0", false, false)
			return recursiveConfig(t, g.URL, a.URL)
		}, true, 1},
		{"recursive-partial", func(t *testing.T) string { return mockConfig(t, true, false) }, true, 3},
		{"workload", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := argoServer(t, "fictional-argo-0", backendOptions{workload: true})
			return configFile(t, g.URL, a.URL)
		}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := tc.config(t)
			dir := t.TempDir()
			markdown, machine := filepath.Join(dir, "preview.md"), filepath.Join(dir, "preview.json")
			args := []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "pr-markdown", "--output", markdown, "--json-output", machine, "--quiet"}
			if tc.recursive {
				args = append(args, "--recursive")
			}
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), args, &stdout, &stderr); code != tc.exit || stdout.Len() != 0 || (tc.exit == 1 && tc.name != "basic-child-skipped" && stderr.Len() != 0) {
				t.Fatalf("example CLI failed: code=%d stderr=%s", code, stderr.String())
			}
			if tc.name == "basic-child-skipped" && (!bytes.Contains(stderr.Bytes(), []byte("analysis_skipped")) || !bytes.Contains(stderr.Bytes(), []byte("not_found (HTTP 404)"))) {
				t.Fatal("basic example hid why child analysis was skipped")
			}
			if tc.exit == 3 {
				if !bytes.Contains(stderr.Bytes(), []byte("access_denied")) || !bytes.Contains(stderr.Bytes(), []byte("Required action:")) {
					t.Fatal("partial result must preserve actionable diagnostics")
				}
				assertNoSecrets(t, "partial result diagnostics", stderr.String(), "fictional-")
			}
			for _, artifact := range []struct{ path, extension string }{{markdown, "md"}, {machine, "json"}} {
				actual, err := os.ReadFile(artifact.path)
				if err != nil {
					t.Fatal(err)
				}
				assertNoSecrets(t, "public example", string(actual), "fictional-")
				if artifact.extension == "json" {
					var report domain.Report
					if err := json.Unmarshal(actual, &report); err != nil || report.Revisions.Base != baseSHA || report.Revisions.Head != headSHA || !report.ChangesFound {
						t.Fatal("example lost its resolved comparison")
					}
				}
			}
		})
	}
}

// impactKeyPaths returns the JSON paths of every object key named impact.
func impactKeyPaths(value any, path string) []string {
	var found []string
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			if key == "impact" {
				found = append(found, path+"/"+key)
			}
			found = append(found, impactKeyPaths(child, path+"/"+key)...)
		}
	case []any:
		for i, child := range x {
			found = append(found, impactKeyPaths(child, fmt.Sprintf("%s/%d", path, i))...)
		}
	}
	return found
}

// Reports describe target differences without an impact field. A run that
// renders workloads and Services, and a recursive run, must encode no
// `impact` key at the top level, per Application or anywhere below.
func TestJSONReportHasNoImpactField(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    func(*testing.T) string
		recursive bool
	}{
		{"workload", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := argoServer(t, "fictional-argo-0", backendOptions{workload: true})
			return configFile(t, g.URL, a.URL)
		}, false},
		{"recursive-change", func(t *testing.T) string {
			g := gitServer(t)
			a, _ := recursiveServer(t, "normal", "fictional-argo-0", false, false)
			return recursiveConfig(t, g.URL, a.URL)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := filepath.Join(t.TempDir(), "preview.json")
			args := []string{"diff", "--config", tc.config(t), "--base", "main", "--head", "feature", "--format", "json", "--output", machine, "--quiet"}
			if tc.recursive {
				args = append(args, "--recursive")
			}
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), args, &stdout, &stderr); code != 1 {
				t.Fatalf("fixture precondition: exit %d, want changes found (1): %s", code, stderr.String())
			}
			data, err := os.ReadFile(machine)
			if err != nil {
				t.Fatal(err)
			}
			var report map[string]any
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			apps, _ := report["applications"].([]any)
			if len(apps) == 0 {
				t.Fatal("fixture precondition: no Applications in the JSON report")
			}
			if keys := impactKeyPaths(report, ""); len(keys) != 0 {
				t.Fatalf("JSON report carries removed impact fields at %v", keys)
			}
		})
	}
}
