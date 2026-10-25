package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reposerver"
)

func failedSetupConfig(t *testing.T, file, mode string) {
	t.Helper()
	dir := filepath.Dir(file)
	contextPath := filepath.Join(dir, "unavailable-context.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "invalid JSON":
		if err = os.WriteFile(contextPath, []byte(`{"credential":"fictional-sensitive-error"`), 0600); err != nil {
			t.Fatal(err)
		}
	case "wrong instance", "missing executable":
		snapshot := reposerver.Snapshot{Version: "v1alpha1", Instance: "instance-0", ContextID: "setup-test", ArgoCDNamespace: "argocd", Bindings: []reposerver.Binding{{Project: "demo", ApplicationNamespace: "argocd", RepositoryURL: fakeRepo, Destination: map[string]any{"name": "test-cluster", "namespace": "demo"}, Request: mockRequestContext()}}}
		if mode == "wrong instance" {
			snapshot.Instance = "instance-1"
		}
		data, _ := json.Marshal(snapshot)
		if err = os.WriteFile(contextPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		if mode == "missing executable" {
			executable = filepath.Join(dir, "fictional-sensitive-error")
		}
	}
	quoted, _ := json.Marshal(executable)
	block := fmt.Sprintf("    tokenEnv: DIFFARO_TEST_ARGO_0\n    repoServer:\n      address: 127.0.0.1:19001\n      plaintext: true\n      grpcurl: %s\n      contextFile: unavailable-context.json", quoted)
	editProfile(t, file, "    tokenEnv: DIFFARO_TEST_ARGO_0", block)
}

func TestBackendSetupFailurePreservesIndependentInstances(t *testing.T) {
	for _, mode := range []string{"missing context", "invalid JSON", "wrong instance", "missing executable"} {
		for _, command := range []string{"diff", "recursive", "doctor", "doctor render"} {
			t.Run(mode+"/"+command, func(t *testing.T) {
				git := gitServer(t)
				first, failedRenders := argoServer(t, "fictional-argo-0", backendOptions{})
				second, healthyRenders := argoServer(t, "fictional-argo-1", backendOptions{workload: true})
				config := configFile(t, git.URL, first.URL, second.URL)
				failedSetupConfig(t, config, mode)
				args := []string{"diff", "--config", config, "--format", "json", "--quiet", "--base", "main", "--head", "feature"}
				switch command {
				case "recursive":
					args = append(args, "--recursive")
				case "doctor":
					args = []string{"doctor", "--config", config, "--format", "json", "--quiet"}
				case "doctor render":
					args[0] = "doctor"
					args = append(args, "--render", "--recursive")
				}
				var output, diagnostic bytes.Buffer
				code := Run(context.Background(), args, &output, &diagnostic)
				var report domain.Report
				if err := json.Unmarshal(output.Bytes(), &report); err != nil || code != 3 || report.Status != "partial_failure" || len(report.Applications) != 2 || len(*failedRenders) != 0 {
					t.Fatalf("setup error aborted the run or fell back to another backend: exit=%d diagnostic=%s err=%v", code, diagnostic.String(), err)
				}
				if report.Applications[0].Execution != "failed" || report.Applications[0].Issues[0].Code != "configuration_invalid" || report.Applications[1].Execution != "complete" {
					t.Fatal("setup failure was hidden or the independent instance was marked failed")
				}
				if command != "doctor" && (len(*healthyRenders) != 2 || len(report.Applications[1].Resources) != 3) {
					t.Fatal("independent target diffs were destroyed by setup failure")
				}
				assertNoSecrets(t, "setup diagnostic", output.String()+diagnostic.String(), "fictional-")
				if !strings.Contains(diagnostic.String(), "Required action:") {
					t.Fatal("setup diagnostic lost its action")
				}
			})
		}
	}
}

func TestAllBackendSetupFailuresKeepConfigurationExit(t *testing.T) {
	git := gitServer(t)
	first, calls := argoServer(t, "fictional-argo-0", backendOptions{})
	config := configFile(t, git.URL, first.URL)
	failedSetupConfig(t, config, "missing context")
	report, code := runJSON(t, config, "--base", "main", "--head", "feature", "--recursive")
	if code != 4 || report.Status != "invalid_configuration" || len(*calls) != 0 {
		t.Fatal("all-invalid preparation changed exit 4 or performed fallback rendering")
	}
}
