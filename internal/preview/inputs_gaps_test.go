package preview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// g53App is an Application that reads chart at the PR base and head.
func g53App(name, chart string, valueFiles ...string) domain.ApplicationResult {
	return domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: name}, State: "unchanged",
		BaseTarget: inputTarget(inputRepo, inputBase, chart, valueFiles...), HeadTarget: inputTarget(inputRepo, inputHead, chart, valueFiles...)}
}

func g53HasLimitation(report domain.Report, fragment string) bool {
	for _, limitation := range report.Limitations {
		if strings.Contains(limitation, fragment) {
			return true
		}
	}
	return false
}

// When a parent renders a child with different helm.values, the
// changed key paths are listed with masked values.
func TestRecursiveChildInlineValuesChangeIsListed(t *testing.T) {
	child := func(values string) domain.Application {
		a := graphApp("api")
		a.Spec["source"].(map[string]any)["helm"] = map[string]any{"values": values}
		return a
	}
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			replicas := "2"
			if revision == graphHead {
				replicas = "3"
			}
			return []string{appManifest(child("image: {tag: v1}\nreplicas: " + replicas + "\n"))}, nil
		}
		return []string{configMap("stable")}, nil
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	api := findApp(t, report, "api")
	var inline *domain.InputChange
	for i := range api.InputChanges {
		if api.InputChanges[i].Kind == "inlineValues" {
			inline = &api.InputChanges[i]
		}
	}
	if inline == nil {
		t.Fatalf("a child's changed helm.values must be listed as an input change: %+v", api.InputChanges)
	}
	if inline.Path != "/spec/source/helm/values" || inline.State != "changed" || inline.Comparison != "complete" || len(inline.Changes) != 1 {
		t.Fatalf("inline values change needs its pointer and key changes: %+v", inline)
	}
	key := inline.Changes[0]
	if key.Path != "/replicas" || !key.Redacted || key.Before != security.Mask || key.After != security.Mask {
		t.Fatalf("changed key path must be shown with masked values: %+v", key)
	}
	if root := findApp(t, report, "root"); len(root.InputChanges) != 0 {
		t.Fatalf("the parent's own spec did not change: %+v", root.InputChanges)
	}
}

// Input changes never change Application states, changesFound or the exit code.
func TestAttachInputChangesKeepsUnchangedState(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values.yaml": "a: 1\n"}, inputHead: {"charts/api/values.yaml": "a: 2\n"}}}
	report := inputReport(g53App("api", "charts/api"))
	exit := report.ExitCode()
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read})
	app := report.Applications[0]
	if len(app.InputChanges) != 1 {
		t.Fatalf("the changed value file must be listed: %+v", app.InputChanges)
	}
	if app.State != "unchanged" || report.ChangesFound || report.ExitCode() != exit {
		t.Fatalf("input evidence changed state %q, changesFound %t or exit %d", app.State, report.ChangesFound, report.ExitCode())
	}
}

// A value file over 1 MiB leaves the comparison incomplete with a reason.
func TestAttachInputChangesOversizedValueFileIsIncomplete(t *testing.T) {
	big := "a: " + strings.Repeat("x", 1<<20) + "\n"
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values.yaml": "a: 1\n"}, inputHead: {"charts/api/values.yaml": big}}}
	report := inputReport(g53App("api", "charts/api"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read})
	got := changesOf(report.Applications[0])["charts/api/values.yaml"]
	if got.Comparison != "incomplete" || !strings.Contains(got.Reason, "exceeds") || len(got.Changes) != 0 {
		t.Fatalf("an oversized value file must be incomplete with a reason, not a diff: %+v", got)
	}
}

// At most 128 value file reads per run; past the limit a value
// file is incomplete with the limit as reason.
func TestAttachInputChangesValueFileReadLimit(t *testing.T) {
	base, head := map[string]string{}, map[string]string{}
	var first []string
	for i := range 65 {
		file := fmt.Sprintf("charts/api/values-%03d.yaml", i)
		base[file], head[file] = "a: 1\n", "a: 2\n"
		if i < 64 {
			first = append(first, file)
		}
	}
	repo := &inputRepository{files: map[string]map[string]string{inputBase: base, inputHead: head}}
	valueNames := func(files []string) []string {
		out := []string{}
		for _, f := range files {
			out = append(out, strings.TrimPrefix(f, "charts/api/"))
		}
		return out
	}
	// Two Applications, so the per-Application input bound does not apply.
	report := inputReport(g53App("api-a", "charts/api", valueNames(first)...), g53App("api-b", "charts/other", "../api/values-064.yaml"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: append(first, "charts/api/values-064.yaml"), Read: repo.read})
	for _, change := range report.Applications[0].InputChanges {
		if change.Comparison != "complete" {
			t.Fatalf("value files within the read limit must be compared: %+v", change)
		}
	}
	got := changesOf(report.Applications[1])["charts/api/values-064.yaml"]
	if got.Comparison != "incomplete" || !strings.Contains(got.Reason, "read limit") || !strings.Contains(got.Reason, "128") {
		t.Fatalf("a value file past the read limit must be incomplete with the limit: %+v", got)
	}
	total := 0
	for key, n := range repo.reads {
		if strings.Contains(key, "/values-") {
			total += n
		}
	}
	if total > 128 {
		t.Fatalf("value file reads exceed the per-run bound: %d", total)
	}
}

// At most 64 inputs per Application, with a limitation.
func TestAttachInputChangesPerApplicationBound(t *testing.T) {
	var files []string
	for i := range 65 {
		files = append(files, fmt.Sprintf("charts/api/templates/t%03d.yaml", i))
	}
	report := inputReport(g53App("api", "charts/api"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: files, Read: (&inputRepository{}).read})
	if got := len(report.Applications[0].InputChanges); got != 64 {
		t.Fatalf("inputs per Application must be bounded to 64, got %d", got)
	}
	if !g53HasLimitation(report, "at most 64 changed input files per Application") {
		t.Fatalf("truncated inputs need a limitation: %v", report.Limitations)
	}
}

// At most 200 key changes per input; more leaves the comparison
// incomplete.
func TestAttachInputChangesKeyChangeBound(t *testing.T) {
	var head strings.Builder
	for i := range 201 {
		fmt.Fprintf(&head, "k%03d: %d\n", i, i)
	}
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values.yaml": "{}\n"}, inputHead: {"charts/api/values.yaml": head.String()}}}
	report := inputReport(g53App("api", "charts/api"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read})
	got := changesOf(report.Applications[0])["charts/api/values.yaml"]
	if len(got.Changes) != 200 || got.Comparison != "incomplete" || !strings.Contains(got.Reason, "more than 200 key changes") {
		t.Fatalf("key changes must be truncated to 200 and marked incomplete: %d %q %q", len(got.Changes), got.Comparison, got.Reason)
	}
}

// At most 256 chart metadata reads per run. Charts past the bound
// cannot be followed, which is stated as a limitation.
func TestAttachInputChangesChartMetadataReadLimit(t *testing.T) {
	base, head := map[string]string{}, map[string]string{}
	var apps []domain.ApplicationResult
	for i := range 70 {
		chart := fmt.Sprintf("charts/c%03d", i)
		base[chart+"/Chart.yaml"], head[chart+"/Chart.yaml"] = "name: c\n", "name: c\n"
		apps = append(apps, g53App(fmt.Sprintf("app-%03d", i), chart))
	}
	repo := &inputRepository{files: map[string]map[string]string{inputBase: base, inputHead: head}}
	report := inputReport(apps...)
	// A changed file outside every chart makes each chart's dependencies
	// followed.
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"docs/readme.md"}, Read: repo.read})
	total := 0
	for _, n := range repo.reads {
		total += n
	}
	if total > 256 {
		t.Fatalf("chart metadata reads exceed the per-run bound: %d", total)
	}
	if !g53HasLimitation(report, "file:// chart dependencies could not be followed") {
		t.Fatalf("charts not followed past the read bound need a limitation: %v", report.Limitations)
	}
}

// A file:// dependency that cannot be followed is stated as a limitation.
func TestAttachInputChangesUnresolvedDependencyLimitation(t *testing.T) {
	chart := "dependencies:\n- name: lib\n  repository: file://../lib\n"
	repo := &inputRepository{fail: "charts/lib/Chart.yaml", files: map[string]map[string]string{inputBase: {"charts/api/Chart.yaml": chart}, inputHead: {"charts/api/Chart.yaml": chart}}}
	report := inputReport(g53App("api", "charts/api"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"charts/lib/templates/_helpers.tpl"}, Read: repo.read})
	if !g53HasLimitation(report, "file:// chart dependencies could not be followed") {
		t.Fatalf("an unreadable dependency Chart.yaml must be stated as a limitation: %v", report.Limitations)
	}
}

// A value file present at base and missing at head is removed.
func TestAttachInputChangesRemovedValueFile(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values-old.yaml": "a: 1\n"}, inputHead: {}}}
	report := inputReport(g53App("api", "charts/api", "values-old.yaml"))
	inputRunner().AttachInputChanges(context.Background(), &report, &InputFiles{Files: []string{"charts/api/values-old.yaml"}, Read: repo.read})
	got := changesOf(report.Applications[0])["charts/api/values-old.yaml"]
	if got.State != "removed" || got.Comparison != "complete" || len(got.Changes) != 1 || got.Changes[0].Path != "/a" || got.Changes[0].Operation != "remove" {
		t.Fatalf("a value file deleted at head must be removed with its keys: %+v", got)
	}
}
