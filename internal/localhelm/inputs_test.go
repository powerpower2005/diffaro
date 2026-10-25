package localhelm

import (
	"context"
	"errors"
	"testing"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
)

// inputsPlan declares well-formed, ref, build-environment, root-relative,
// glob and malformed inputs on one chart source next to a ref source.
func inputsPlan() []domain.SourceRevision {
	return []domain.SourceRevision{
		{Position: 1, Repository: "chart", Revision: "a", Source: map[string]any{"path": "charts/app", "helm": map[string]any{
			"valueFiles":     []any{"values.yaml", "$values/env/prod.yaml", "values-$ARGOCD_APP_NAME.yaml", 42, "*.yaml"},
			"fileParameters": []any{map[string]any{"name": "note", "path": "/files/note.txt"}, "not-an-object"},
		}}},
		{Position: 2, Repository: "values", Revision: "b", Ref: "values", Source: map[string]any{"ref": "values"}},
	}
}

func TestResolveInputsResolvesEveryEntryOnce(t *testing.T) {
	req := Request{Application: domain.Application{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}}, Settings: argocd.RenderSettings{ControllerNamespace: "argocd"}, Plan: inputsPlan()}
	inputs := resolveInputs(req, planRefs(req.Plan))
	type want struct {
		kind               inputKind
		malformed          bool
		ref, repoKey, path string
		readFrom           int
		skipped            bool
		trimmable          bool
	}
	wants := []want{
		{kind: valueFileInput, repoKey: "chart@a", path: "charts/app/values.yaml", trimmable: true},
		{kind: valueFileInput, ref: "$values", repoKey: "values@b", path: "env/prod.yaml", readFrom: 1, trimmable: true},
		{kind: valueFileInput, repoKey: "chart@a", path: "charts/app/values-api.yaml"},
		{kind: valueFileInput, malformed: true, repoKey: "chart@a"},
		{kind: valueFileInput, repoKey: "chart@a", skipped: true},
		{kind: fileParameterInput, repoKey: "chart@a", path: "files/note.txt"},
		{kind: fileParameterInput, malformed: true, repoKey: "chart@a"},
	}
	if len(inputs) != len(wants) {
		t.Fatalf("resolved %d inputs, want %d: %+v", len(inputs), len(wants), inputs)
	}
	for i, w := range wants {
		in := inputs[i]
		var skipped *domain.RenderSkipped
		if in.Source != 0 || in.Kind != w.kind || in.Malformed != w.malformed || in.Ref != w.ref || in.RepoKey != w.repoKey || in.Path != w.path || in.ReadFrom != w.readFrom || errors.As(in.Err, &skipped) != w.skipped || in.trimmable() != w.trimmable {
			t.Errorf("input %d = %+v, want %+v", i, in, w)
		}
	}
	if inputs[5].Name != "note" {
		t.Errorf("file parameter name not kept: %+v", inputs[5])
	}
	// The render resolves "$values/" as a ref only when that repository has
	// files; otherwise "$values" is an unavailable build variable.
	var skipped *domain.RenderSkipped
	if without := resolveInputs(req, map[string]int{}); without[1].Ref != "" || !errors.As(without[1].Err, &skipped) {
		t.Fatalf("ref without files resolved as a ref: %+v", without[1])
	}
}

func TestInputConsumersShareResolvedPaths(t *testing.T) {
	plan := inputsPlan()
	req := Request{Application: domain.Application{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}}, Settings: argocd.RenderSettings{ControllerNamespace: "argocd"}, Plan: plan}
	needs := planNeeds(req)
	chart, values := needs["chart@a"], needs["values@b"]
	for _, name := range []string{"charts/app/values.yaml", "charts/app/values-api.yaml", "files/note.txt"} {
		if !chart.files[name] {
			t.Errorf("planNeeds misses resolved input %s", name)
		}
	}
	if len(chart.files) != 3 || len(values.files) != 1 || !values.files["env/prod.yaml"] {
		t.Fatalf("planNeeds must hold exactly the resolved paths: chart=%v values=%v", chart.files, values.files)
	}
	// A link on the ref path counts in the ref repository only.
	if err := linkedInputs(req, [][]string{nil, {"env"}}); err == nil {
		t.Fatal("linked ref input was not detected in the ref repository")
	}
	if err := linkedInputs(req, [][]string{{"env"}, nil}); err != nil {
		t.Fatalf("ref input was checked against the chart repository: %v", err)
	}
	// Build-environment and absolute inputs make the plan untrimmable.
	if trimmablePlan(plan) {
		t.Fatal("plan with build-environment and absolute inputs was trimmable")
	}
	plan[0].Source["helm"] = map[string]any{"valueFiles": []any{"values.yaml", "$values/env/prod.yaml"}}
	if !trimmablePlan(plan) {
		t.Fatal("plan with only plain and ref inputs was not trimmable")
	}
}

// Dependency discovery is lenient for sparse reads, which fetch more files
// when a chart file does not parse, and strict for trimming, which must not
// drop files the chart loader might need.
func TestFileDependencyDiscoveryKeepsEachCallersParseChoice(t *testing.T) {
	broken := map[string]string{
		"syntax": "dependencies: [",
		// Valid YAML whose name the chart loader cannot read as a string.
		"loader type": "apiVersion: v2\nname: [api]\nversion: 0.1.0\ndependencies:\n  - name: common\n    repository: file://../../libs/common\n",
	}
	for name, chartFile := range broken {
		t.Run(name, func(t *testing.T) {
			git := largeRepository()
			git.files["apps/api/Chart.yaml"] = chartFile
			git.files["apps/api/requirements.yaml"] = "dependencies:\n  - name: common\n    repository: file://../../libs/common\n"
			c := &Client{Git: git, SnapshotLimit: 64 << 20}
			need := &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}}
			files, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, need)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := files["libs/common/Chart.yaml"]; !ok || git.archives != 0 {
				t.Fatalf("sparse read skipped the declared file:// dependency: archives=%d", git.archives)
			}
			if _, ok := trimFiles(files, need); ok {
				t.Fatal("trimming accepted chart metadata the chart loader cannot parse")
			}
		})
	}
}
