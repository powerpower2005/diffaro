package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

func TestTerminalCLIAndJSONUseSameComparison(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "partial"}[partial], func(t *testing.T) {
			g := gitServer(t)
			a, revisions := argoServer(t, "fictional-argo-0", backendOptions{workload: true})
			endpoints := []string{a.URL}
			if partial {
				denied, _ := argoServer(t, "fictional-argo-1", backendOptions{denied: true})
				endpoints = append(endpoints, denied.URL)
			}
			config := configFile(t, g.URL, endpoints...)
			jsonFile := filepath.Join(t.TempDir(), "preview.json")
			var out, stderr bytes.Buffer
			code := Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "terminal", "--json-output", jsonFile, "--quiet"}, &out, &stderr)
			wantCode := 1
			if partial {
				wantCode = 3
			}
			if code != wantCode {
				t.Fatal(code, stderr.String())
			}
			data, err := os.ReadFile(jsonFile)
			var report domain.Report
			if err != nil || json.Unmarshal(data, &report) != nil {
				t.Fatal("JSON sidecar unavailable", err)
			}
			want, err := reporter.Encode(report, "terminal", security.Redactor{})
			if err != nil || !bytes.Equal(want, out.Bytes()) || report.ExitCode() != code {
				t.Fatal("terminal and JSON results diverged")
			}
			if !renderedOnceEach(*revisions, baseSHA, headSHA) {
				t.Fatal("output option triggered another comparison")
			}
			for _, want := range []string{"Diffaro target preview", "Deployment/api", "/spec/replicas: 3 -> 1", "Redaction: enabled", headSHA} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q", want)
				}
			}
			if partial && (!strings.Contains(out.String(), "Required access:") || !strings.Contains(out.String(), "partial_failure") || !strings.Contains(stderr.String(), "Required action:")) {
				t.Fatal("partial failure lost actionable results")
			}
			assertNoSecrets(t, "terminal output", out.String()+stderr.String(), "fictional-")
			assertNoSecrets(t, "terminal output or saved report", string(data)+out.String(), "++++++++")
		})
	}
}

func TestDoctorAcceptsTerminal(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	config := configFile(t, g.URL, a.URL)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--config", config, "--render", "--base", "main", "--head", "feature", "--format", "terminal", "--quiet"}, &out, &stderr)
	if code != 1 || !strings.Contains(out.String(), "Diffaro target preview") || !strings.Contains(out.String(), "/spec/replicas: 2 -> 3") {
		t.Fatal(code, out.String(), stderr.String())
	}
}

func TestTerminalMaskedSecretRemainsUnknown(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{secret: true})
	config := configFile(t, g.URL, a.URL)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "terminal", "--quiet"}, &out, &stderr)
	for _, want := range []string{"Secret/credentials", "unknown", "/spec/replicas: 2 -> 3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q\n%s", want, out.String())
		}
	}
	if code != 1 || strings.Contains(out.String()+stderr.String(), "++++++++") {
		t.Fatal("masked Secret was treated as unchanged or exposed", code)
	}
}
