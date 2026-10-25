package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func inputChangeReport() domain.Report {
	r := domain.NewReport()
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: "base", Head: "head", Mode: "pr-merge-base-head"}
	shared := domain.InputChange{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete", Changes: []domain.Change{{Path: "/replicaCount", Operation: "replace", Before: 2, After: 3}}}
	changed := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api-prod"}, State: "changed", Execution: "complete", InputChanges: []domain.InputChange{shared},
		Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Cluster: "a:c", Group: "apps", Kind: "Deployment", Namespace: "api", Name: "api"}, State: "changed", Comparison: "complete", Changes: []domain.Change{{Path: "/spec/replicas", Operation: "replace", Before: 2, After: 3}}}}}
	// Same input, no rendered change: the reviewer must still see it.
	unchanged := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api-stg"}, State: "unchanged", Execution: "complete", InputChanges: []domain.InputChange{shared}, Resources: []domain.ResourceDiff{}}
	r.Applications = []domain.ApplicationResult{changed, unchanged}
	r.ChangesFound = true
	return r
}

func TestInputChangesAppearNextToTargetChanges(t *testing.T) {
	r := inputChangeReport()
	pr, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(pr)
	for _, want := range []string{
		"PR inputs these Applications read: 1 changed input(s), read by 2 Application(s)",
		"- `charts/api/values.yaml` (value file, changed, 1 key change(s)) → `a/argocd/api-prod`, `a/argocd/api-stg`",
		"  - `/replicaCount`: `2` → `3`",
		"they do not establish which target field a key produced",
		"**PR inputs changed, rendered targets unchanged (1):** `a/argocd/api-stg`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("PR Markdown is missing %q:\n%s", want, text)
		}
	}
	full, _ := Encode(r, "markdown", security.Redactor{})
	if !strings.Contains(string(full), "Changed render inputs:") || !strings.Contains(string(full), "`/replicaCount`") {
		t.Fatalf("full Markdown needs per-Application inputs:\n%s", full)
	}
	terminal, _ := Encode(r, "terminal", security.Redactor{})
	if !strings.Contains(string(terminal), "App a/argocd/api-stg") || !strings.Contains(string(terminal), "Input charts/api/values.yaml (value file, changed, 1 key change(s))") {
		t.Fatalf("terminal must show an Application whose only change is an input:\n%s", terminal)
	}
	again, _ := Encode(r, "pr-markdown", security.Redactor{})
	if string(again) != text {
		t.Fatal("input summary must be deterministic")
	}
}

// The final report pass redacts value file changes for value files, so keys
// that name a Secret stay visible, while inline values stay masked whole.
func TestValueFileReferencesSurviveTheReportPass(t *testing.T) {
	r := inputChangeReport()
	r.Applications[0].InputChanges = []domain.InputChange{
		{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete", Changes: []domain.Change{
			{Path: "/slack", Operation: "add", After: map[string]any{"enabled": true, "secretName": "app-secrets", "secretKey": "fictional-key"}},
		}},
		{Kind: "inlineValues", Path: "/spec/source/helm/values", State: "changed", Comparison: "complete", Changes: []domain.Change{
			{Path: "/slack", Operation: "add", After: map[string]any{"secretName": "fictional-inline"}},
		}},
	}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if !strings.Contains(out, "app-secrets") || strings.Contains(out, "fictional-") {
			t.Fatalf("%s hid a value file reference or exposed a value:\n%s", format, out)
		}
	}
}
