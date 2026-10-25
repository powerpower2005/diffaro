package reporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

func containers(namesAndImages ...string) []any {
	var out []any
	for i := 0; i < len(namesAndImages); i += 2 {
		out = append(out, map[string]any{"name": namesAndImages[i], "image": namesAndImages[i+1]})
	}
	return out
}

func TestImageSummaryUsesNamesAndKnownWorkloadPaths(t *testing.T) {
	for _, tc := range []struct{ group, kind, path string }{
		{"apps", "Deployment", "/spec/template/spec/containers"},
		{"apps", "StatefulSet", "/spec/template/spec/initContainers"},
		{"apps", "DaemonSet", "/spec/template/spec/containers"},
		{"batch", "Job", "/spec/template/spec/containers"},
		{"batch", "CronJob", "/spec/jobTemplate/spec/template/spec/containers"},
		{"", "Pod", "/spec/ephemeralContainers"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			resource := domain.ResourceDiff{ID: domain.ResourceID{Group: tc.group, Kind: tc.kind}, State: "changed", Comparison: "complete", Changes: []domain.Change{{Path: tc.path, Operation: "replace", Before: containers("api", "example/api:v1", "sidecar", "example/sidecar:v1"), After: containers("sidecar", "example/sidecar:v1", "api", "example/api:v2", "added", "example/new:v1")}}}
			changes := imageChanges(resource)
			if len(changes) != 1 || changes[0].name != "api" || changes[0].before != "example/api:v1" || changes[0].after != "example/api:v2" {
				t.Fatalf("image projection confused order, addition or workload path: %#v", changes)
			}
		})
	}
}

func TestImageSummaryIsBoundedAndEscaped(t *testing.T) {
	report := domain.NewReport()
	for i := 0; i < 15; i++ {
		report.Applications = append(report.Applications, domain.ApplicationResult{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: fmt.Sprintf("app-%02d", i)}, Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "api"}, State: "changed", Comparison: "complete", Changes: []domain.Change{{Path: "/spec/template/spec/containers", Before: containers("api", "example/api:v1"), After: containers("api", "example/api:<script>`|v2")}}}}})
	}
	// Full Markdown keeps the image summary; PR Markdown shows each image
	// change in its resource's diff block.
	output, err := Encode(report, "markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(output), "`containers/api`") != 12 || !strings.Contains(string(output), "3 more image changes") || strings.Contains(string(output), "<script>") || !strings.Contains(string(output), "&lt;script&gt;&#96;&#124;") {
		t.Fatal("image summary is unbounded or accepts markup")
	}
	output, err = Encode(report, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	visible := strings.Split(string(output), "<details>")[0]
	if strings.Count(visible, "+  image: example/api:<script>`|v2\n") != 15 || strings.Contains(outsideFences(visible), "<script>") || strings.Contains(visible, "Container image changes") || len(output) > MaxPRMarkdownBytes {
		t.Fatalf("PR Markdown hid an image change, accepted markup or repeated the summary:\n%s", visible)
	}
}

func TestImageSummaryDoesNotInferUnavailableOrUnchangedImages(t *testing.T) {
	for _, tc := range []struct {
		name, group, kind, state, comparison, path string
		before, after                              any
		redacted                                   bool
	}{
		{"reorder", "apps", "Deployment", "changed", "complete", "/spec/template/spec/containers", containers("a", "a:v1", "b", "b:v1"), containers("b", "b:v1", "a", "a:v1"), false},
		{"duplicate", "apps", "Deployment", "changed", "complete", "/spec/template/spec/containers", containers("a", "a:v1", "a", "a:v0"), containers("a", "a:v2"), false},
		{"masked", "apps", "Deployment", "changed", "complete", "/spec/template/spec/containers", containers("a", security.Mask), containers("a", "a:v2"), false},
		{"redacted field", "apps", "Deployment", "changed", "complete", "/spec/template/spec/containers", containers("a", "a:v1"), containers("a", "a:v2"), true},
		{"missing", "apps", "Deployment", "changed", "complete", "/spec/template/spec/containers", nil, containers("a", "a:v2"), false},
		{"unknown", "example.invalid", "Deployment", "changed", "complete", "/spec/template/spec/containers", containers("a", "a:v1"), containers("a", "a:v2"), false},
		{"wrong path", "apps", "Deployment", "changed", "complete", "/data/containers", containers("a", "a:v1"), containers("a", "a:v2"), false},
		{"incomplete", "apps", "Deployment", "changed", "incomplete", "/spec/template/spec/containers", containers("a", "a:v1"), containers("a", "a:v2"), false},
		{"removed", "apps", "Deployment", "removed", "complete", "/spec/template/spec/containers", containers("a", "a:v1"), containers("a", "a:v2"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource := domain.ResourceDiff{ID: domain.ResourceID{Group: tc.group, Kind: tc.kind}, State: domain.State(tc.state), Comparison: domain.Comparison(tc.comparison), Changes: []domain.Change{{Path: tc.path, Before: tc.before, After: tc.after, Redacted: tc.redacted}}}
			if len(imageChanges(resource)) != 0 {
				t.Fatal("summary inferred an image update without complete evidence")
			}
		})
	}
}

func TestImageSummaryKeepsFullDiffAndJSONWithRedactionAndEscaping(t *testing.T) {
	render := func(image string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: demo\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: " + image + "\n          env:\n            - name: API_TOKEN\n              value: fictional-container-token\n"
	}
	base, err := manifest.Parse([]string{render("registry.example.invalid/api:v1")}, "cluster", "demo", manifest.Limits{Bytes: 1 << 20, Documents: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := manifest.Parse([]string{render("registry.example.invalid/api:v2")}, "cluster", "demo", manifest.Limits{Bytes: 1 << 20, Documents: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resources, _ := manifest.Compare(base, head, security.Redactor{})
	report := domain.NewReport()
	report.Applications = []domain.ApplicationResult{{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}, Resources: resources}}
	for _, format := range []string{"markdown", "pr-markdown"} {
		output, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		visible := strings.Split(string(output), "<details>")[0]
		// PR Markdown shows the image change in the resource's diff block.
		shown := strings.Contains(visible, "`containers/api`: `registry.example.invalid/api:v1` → `registry.example.invalid/api:v2`")
		if format == "pr-markdown" {
			// The compared resource has YAML line hunks.
			shown = strings.Contains(visible, "@@ -11 +11 @@ /spec/template/spec/containers[api]/image\n") && strings.Contains(visible, "\n-          image: registry.example.invalid/api:v1\n+          image: registry.example.invalid/api:v2\n")
		}
		if !shown || strings.Contains(string(output), "fictional-container-token") || !strings.Contains(string(output), "/spec/template/spec/containers") {
			t.Fatal("image update is hidden, original evidence lost or secret leaked")
		}
	}
	before, _ := json.Marshal(report)
	output, err := Encode(report, "json", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	var machine domain.Report
	// Containers are compared by name: one image field change.
	if json.Unmarshal(output, &machine) != nil || len(machine.Applications[0].Resources[0].Changes) != 1 || machine.Applications[0].Resources[0].Changes[0].Path != "/spec/template/spec/containers/0/image" || fmt.Sprint(machine.Applications[0].Resources[0].Changes[0].ItemNames) != "[api]" {
		t.Fatal("machine diff changed")
	}
	after, _ := json.Marshal(report)
	if !bytes.Equal(before, after) {
		t.Fatal("display mutated the report")
	}
	for _, format := range []string{"markdown", "pr-markdown"} {
		output, _ := Encode(report, format, security.Redactor{Secrets: []string{"registry.example.invalid/api:v1"}})
		if strings.Contains(string(output), "registry.example.invalid/api:v1") || strings.Contains(string(output), "Container image changes") {
			t.Fatal("masked image was projected")
		}
	}
}
