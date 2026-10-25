package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func discoveryInventoryServer(t *testing.T, instance int, mode string) *httptest.Server {
	t.Helper()
	base, _ := argoServer(t, fmt.Sprintf("fictional-argo-%d", instance), backendOptions{workload: true})
	var inventories atomic.Int32
	return overlayArgoCD(t, base, map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer fictional-argo-%d", instance) {
			t.Error("inventory crossed credential boundary")
		}
		count := inventories.Add(1)
		if mode == "denied" {
			http.Error(w, "fictional-sensitive-error", http.StatusForbidden)
			return
		}
		application := testApp()
		if mode == "drift" && count > 1 {
			application["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["releaseName"] = "changed-after-preview"
		}
		writeInventory(w, application)
	}})
}

func TestCLISelectionFailurePreservesIndependentResults(t *testing.T) {
	for _, mode := range []string{"denied", "drift"} {
		t.Run(mode, func(t *testing.T) {
			git := gitServer(t)
			first := discoveryInventoryServer(t, 0, mode)
			second := discoveryInventoryServer(t, 1, "stable")
			file := configFile(t, git.URL, first.URL, second.URL)
			editProfile(t, file, "applications:\n  - instance: instance-0\n    namespace: argocd\n    name: api\n  - instance: instance-1\n    namespace: argocd\n    name: api\n", "discovery:\n  mode: repository\n")
			report, code := runJSON(t, file, "--base", "main", "--head", "feature")
			if code != 3 || report.Status != "partial_failure" || report.Graph.BaseComplete || report.Graph.HeadComplete {
				t.Fatalf("failed inventory advertised complete discovery: exit=%d", code)
			}
			if report.Discovery[0].Status != "failed" || report.Discovery[1].Status != "complete" || multiInstanceResult(t, report, "instance-1", "api").Execution != "complete" {
				t.Fatal("independent selection or resource comparison marked failed")
			}
		})
	}
}
