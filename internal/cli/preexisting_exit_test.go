package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/policy"
	"diffaro/internal/preview"
)

// TestPolicyLeavesOutPreexistingFailuresWithTheOptIn follows the CLI order: preexisting failures are excluded, then policies run. An
// Application failing on both revisions has no rendered target the PR could
// remove, so with the opt-in it is not evaluated; every other Application
// still is, and any other failure keeps the evaluation incomplete.
func TestPolicyLeavesOutPreexistingFailuresWithTheOptIn(t *testing.T) {
	old := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "old"}
	other := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "other"}
	healthy := func(state string) domain.ApplicationResult {
		return domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, Project: "demo", State: "changed", Execution: "complete", Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Cluster: "east:test", Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "api"}, State: domain.State(state), Comparison: "complete"}}}
	}
	failed := func(id domain.ApplicationID, origin string) domain.ApplicationResult {
		return domain.ApplicationResult{ID: id, State: "unknown", Execution: "failed", FailureOrigin: domain.FailureOrigin(origin), Issues: []domain.Issue{{Code: "local_render_failed", Stage: "render", Reason: "missing", Application: &id}}}
	}
	rule := []domain.PolicyRule{{ID: "keep-api", Rule: "deny-resource-removal-v1", Match: domain.PolicySelector{Group: "apps", Kind: "Deployment"}}}
	for _, tc := range []struct {
		name       string
		optIn      bool
		apps       []domain.ApplicationResult
		status     string
		evaluation string
	}{
		{"a removal in a healthy Application is still a violation", true, []domain.ApplicationResult{healthy("removed"), failed(old, "preexisting")}, "policy_violation", "complete"},
		{"no removal: the run passes with a complete evaluation", true, []domain.ApplicationResult{healthy("changed"), failed(old, "preexisting")}, "success", "complete"},
		{"another failure keeps the evaluation incomplete", true, []domain.ApplicationResult{healthy("changed"), failed(old, "preexisting"), failed(other, "both-sides")}, "partial_failure", "incomplete"},
		{"without the opt-in the evaluation stays incomplete", false, []domain.ApplicationResult{healthy("changed"), failed(old, "preexisting")}, "partial_failure", "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := domain.NewReport()
			r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
			r.Status, r.ChangesFound, r.Applications = "partial_failure", true, tc.apps
			// The failed Applications also leave graph discovery incomplete.
			r.Graph = &domain.GraphSummary{BaseComplete: false, HeadComplete: false}
			for _, app := range tc.apps {
				r.Issues = append(r.Issues, app.Issues...)
			}
			if tc.optIn {
				preview.ExcludePreexistingFailures(&r)
			}
			policy.Apply(&r, rule)
			if string(r.Status) != tc.status || r.Policy.Evaluation != tc.evaluation {
				t.Fatalf("status=%s evaluation=%s, want %s %s", r.Status, r.Policy.Evaluation, tc.status, tc.evaluation)
			}
			for _, violation := range r.Policy.Violations {
				if violation.Application == old {
					t.Fatal("an Application failing on both revisions was evaluated")
				}
			}
		})
	}
}

// preexistingFailureProfile builds a changed-path discovery profile for PR 12.
// The PR changes charts/api/values.yaml only. Application "api" renders a
// change; Application "old" reads charts/old, which the PR does not touch,
// and Argo CD fails to render it at base and head with the same missing
// path. Its unknown child status makes it an ineligible candidate that is
// still rendered, with its unchanged inputs recorded.
func preexistingFailureProfile(t *testing.T, optIn bool) string {
	t.Helper()
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	chart := func(name string) string {
		return base64.StdEncoding.EncodeToString([]byte("apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n"))
	}
	g := newGitHubFake(t, githubFake{token: "fictional-github-token", routes: func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/repos/example/charts/pulls/12" && r.URL.Query().Get("page") == "":
			fmt.Fprintf(w, `{"changed_files":1,"base":{"sha":%q,"repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, baseSHA, headSHA)
		case r.URL.Path == "/repos/example/charts/pulls/12/files":
			fmt.Fprint(w, `[{"filename":"charts/api/values.yaml"}]`)
		case r.URL.Path == "/repos/example/charts/git/trees/"+headSHA:
			fmt.Fprintf(w, `{"truncated":false,"tree":[{"path":"charts/api/Chart.yaml","mode":"100644","type":"blob","sha":%q},{"path":"charts/old/Chart.yaml","mode":"100644","type":"blob","sha":%q}]}`, strings.Repeat("b", 40), strings.Repeat("d", 40))
		case r.URL.Path == "/repos/example/charts/contents/charts/api/Chart.yaml":
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, chart("api"))
		case r.URL.Path == "/repos/example/charts/contents/charts/old/Chart.yaml":
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, chart("old"))
		case r.URL.Path == "/repos/example/charts/contents/charts/api/values.yaml":
			values := "replicaCount: 1\n"
			if r.URL.Query().Get("ref") == headSHA {
				values = "replicaCount: 2\n"
			}
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(values)))
		case strings.HasPrefix(r.URL.Path, "/repos/example/charts/contents/"):
			http.NotFound(w, r)
		default:
			return false
		}
		return true
	}})
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	api := testApp()
	api["status"] = map[string]any{"resources": []any{map[string]any{"group": "apps", "kind": "Deployment"}}}
	old := map[string]any{
		"metadata": map[string]any{"name": "old", "namespace": "argocd"},
		"spec":     map[string]any{"project": "demo", "source": map[string]any{"repoURL": fakeRepo, "path": "charts/old", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "test-cluster", "namespace": "demo"}},
	}
	server := overlayArgoCD(t, a, map[string]http.HandlerFunc{
		"/api/v1/applications":     func(w http.ResponseWriter, r *http.Request) { writeInventory(w, api, old) },
		"/api/v1/applications/old": func(w http.ResponseWriter, r *http.Request) { writeJSON(w, old) },
		"/api/v1/applications/old/manifests": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": "charts/old: app path does not exist", "code": 2, "message": "charts/old: app path does not exist"})
		},
	})
	path := filepath.Join(t.TempDir(), "profile.yaml")
	var out, diagnostic bytes.Buffer
	args := []string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--config", path, "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB", "--selection", "changedPaths"}
	if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
		t.Fatalf("init failed: %d %s", code, diagnostic.String())
	}
	if optIn {
		appendProfile(t, path, "failures:\n  excludePreexistingFromExit: true\n")
	}
	return path
}

func applicationNamed(t *testing.T, report domain.Report, name string) domain.ApplicationResult {
	t.Helper()
	for _, app := range report.Applications {
		if app.ID.Name == name {
			return app
		}
	}
	t.Fatalf("missing Application %s in %+v", name, report.Applications)
	return domain.ApplicationResult{}
}

// TestCLIAnnotatesPreexistingFailureOrigin runs the CLI with a deterministic
// failure at base and head of an Application whose inputs the PR does not
// change: the JSON artifact labels it preexisting and, without
// the opt-in, it still counts as a partial failure.
func TestCLIAnnotatesPreexistingFailureOrigin(t *testing.T) {
	path := preexistingFailureProfile(t, false)
	var out, errout bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--config", path, "--format", "json", "--quiet", "--pr", "12"}, &out, &errout)
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (partial failure) without the opt-in: %s", code, errout.String())
	}
	var raw struct {
		Applications []map[string]any `json:"applications"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, app := range raw.Applications {
		id, _ := app["id"].(map[string]any)
		if id["name"] == "old" {
			found = true
			if app["failureOrigin"] != "preexisting" || app["execution"] != "failed" {
				t.Fatalf("JSON failureOrigin=%v execution=%v, want preexisting failed", app["failureOrigin"], app["execution"])
			}
		}
	}
	if !found {
		t.Fatalf("failed Application missing from JSON: %s", out.String())
	}
	var report domain.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.PreexistingFailuresExcluded != 0 || report.Status != "partial_failure" {
		t.Fatalf("default run excluded failures: status=%s excluded=%d", report.Status, report.PreexistingFailuresExcluded)
	}
	if applicationNamed(t, report, "api").State != "changed" {
		t.Fatal("the healthy Application's change was lost")
	}
}

// TestCLIOptInExcludesPreexistingFailureFromExit verifies exit behavior:
// the same run exits 3 by default and, with
// failures.excludePreexistingFromExit, exits 1 for the healthy change while
// the failure stays in the report with the notice.
func TestCLIOptInExcludesPreexistingFailureFromExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		optIn    bool
		code     int
		status   string
		excluded int
	}{
		{"default counts the preexisting failure", false, 3, "partial_failure", 0},
		{"opt-in leaves it out of the exit code", true, 1, "success", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// runJSON rejects diagnostics on success; the opt-in run must print
			// the exclusion next to the status, so it is read here instead.
			var out, errout bytes.Buffer
			code := Run(context.Background(), []string{"diff", "--config", preexistingFailureProfile(t, tc.optIn), "--format", "json", "--quiet", "--pr", "12"}, &out, &errout)
			var report domain.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if code != tc.code || string(report.Status) != tc.status || report.PreexistingFailuresExcluded != tc.excluded {
				t.Fatalf("exit=%d status=%s excluded=%d, want %d %s %d", code, report.Status, report.PreexistingFailuresExcluded, tc.code, tc.status, tc.excluded)
			}
			old := applicationNamed(t, report, "old")
			if old.Execution != "failed" || old.FailureOrigin != "preexisting" || len(old.Issues) == 0 {
				t.Fatalf("the preexisting failure must stay in the report: %+v", old)
			}
			noticed := false
			for _, limitation := range report.Limitations {
				noticed = noticed || strings.Contains(limitation, "were already failing before this PR; they are excluded from the exit code by failures.excludePreexistingFromExit")
			}
			if noticed != tc.optIn {
				t.Fatalf("exclusion notice present=%t, want %t: %v", noticed, tc.optIn, report.Limitations)
			}
			console := errout.String()
			if strings.Contains(console, "Not counted: 1 Application(s) already failing before this PR (failures.excludePreexistingFromExit)") != tc.optIn {
				t.Fatalf("console verdict exclusion line present=%t, want %t:\n%s", !tc.optIn, tc.optIn, console)
			}
			if !strings.Contains(console, "Failure origin: already failing before this PR") || !strings.Contains(console, "Required action:") {
				t.Fatalf("console diagnostics must keep the failure and its origin:\n%s", console)
			}
		})
	}
}
