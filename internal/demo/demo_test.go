package demo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestDemoRealTargetsAndBoundaries(t *testing.T) {
	for _, scenario := range []string{"new-app", "partial-failure", "skipped-context"} {
		t.Run(scenario, func(t *testing.T) {
			r, err := Run(context.Background(), scenario)
			if err != nil || r.Revisions.Mode != "demo-fixture-base-head" || r.RenderBackends[0].Verification != "demo-fixture" || !strings.HasPrefix(r.Limitations[0], "DEMO:") {
				t.Fatal("demo lost its synthetic provenance", err)
			}
			// The demo command has no --pr, so it must not ask for one.
			for _, limitation := range r.Limitations {
				if strings.Contains(limitation, "--pr") {
					t.Fatal("demo report asks for a pull request:", limitation)
				}
			}
			apps := map[string]domain.ApplicationResult{}
			for _, a := range r.Applications {
				apps[a.ID.Name] = a
			}
			if apps["api"].Execution != "complete" || len(apps["api"].Resources) != 3 {
				t.Fatal("independent resource changes lost")
			}
			worker := apps["worker"]
			if worker.State != "added" || worker.HeadTarget == nil || worker.HeadTarget.Revision != headSHA {
				t.Fatal("new child revision/provenance lost", worker)
			}
			if scenario == "skipped-context" {
				if worker.Execution != "skipped" || len(worker.Resources) != 0 || !strings.Contains(worker.SkipReason, "cluster read access") || r.Graph.HeadComplete || r.ExitCode() != 1 {
					t.Fatal("unavailable targets must remain unknown", worker)
				}
			} else {
				kinds := map[string]bool{}
				for _, resource := range worker.Resources {
					if resource.State != "added" || resource.ID.Name != "worker" || resource.ID.Namespace != "demo" {
						t.Fatal("incorrect new workload", resource)
					}
					kinds[resource.ID.Kind] = true
				}
				if worker.Execution != "complete" || len(kinds) != 3 || !kinds["Deployment"] || !kinds["Service"] || !kinds["ConfigMap"] {
					t.Fatal("new child internal targets missing", worker)
				}
			}
			if scenario == "partial-failure" {
				broken := apps["broken"]
				if r.ExitCode() != 3 || r.Status != "partial_failure" || broken.Execution != "failed" || len(broken.Resources) != 0 || len(broken.Issues) == 0 || broken.Issues[0].Stage != "render" {
					t.Fatal("failed branch hidden or treated as empty", broken)
				}
				// The production steps after a comparison label the failure
				// as a real run would: the base renders, the head fails.
				if broken.FailureOrigin != domain.FailureIntroduced {
					t.Fatal("demo skipped the production failure origin", broken.FailureOrigin)
				}
			} else if r.ExitCode() != 1 {
				t.Fatal("changes exit lost", r.ExitCode())
			}
			if scenario == "new-app" && (!r.Graph.BaseComplete || !r.Graph.HeadComplete) {
				t.Fatal("complete fixture graph unexpectedly incomplete")
			}
			// Render twice, rather than snapshotting hand-written reports.
			again, err := Run(context.Background(), scenario)
			first, _ := json.Marshal(r)
			second, _ := json.Marshal(again)
			if err != nil || string(first) != string(second) {
				t.Fatal("demo is nondeterministic", err)
			}
		})
	}
}

func TestDemoInvalidInputAndCancellation(t *testing.T) {
	if _, err := Run(context.Background(), "untrusted-chart"); err == nil {
		t.Fatal("arbitrary fixture accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := Run(ctx, "new-app")
	if err != nil || r.ExitCode() != 7 {
		t.Fatal("cancellation lost", err, r.Status)
	}
}
