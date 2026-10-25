package preview

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

// After every comparison, a value masked in a rendered target is masked
// where it reappears in another resource's changes and line hunks and in an
// Application's spec changes, also inside a change already partly redacted;
// other values, numbers and unaffected hunks stay.
func TestMaskRepeatedValuesMasksResourcesAndSpecChanges(t *testing.T) {
	masked := &security.MaskedValues{}
	masked.Add([]any{"fictional-shared-key", "postgres://u:fictional-embedded@db/app"})
	hunks := []domain.LineHunk{{Path: "/data", Lines: []string{"-  copy: old-visible", "+  copy: fictional-shared-key"}}}
	untouched := []domain.LineHunk{{Path: "/spec", Lines: []string{"-  replicas: 2", "+  replicas: 3"}}}
	report := domain.Report{Applications: []domain.ApplicationResult{{
		SpecChanges: []domain.Change{{Path: "/spec/source/helm/releaseName", Operation: domain.OperationReplace, Before: "api", After: "fictional-shared-key"}},
		Resources: []domain.ResourceDiff{
			{ID: domain.ResourceID{Kind: "ConfigMap", Name: "cm"}, LineHunks: hunks, Changes: []domain.Change{
				{Path: "/data/copy", Operation: domain.OperationReplace, Before: "old-visible", After: "fictional-shared-key"},
				{Path: "/data/env", Operation: domain.OperationReplace, Redacted: true, Before: map[string]any{"token": security.Mask, "pass": "fictional-embedded"}, After: map[string]any{"token": security.Mask}},
				{Path: "/data/plain", Operation: domain.OperationReplace, Before: "a", After: "b"},
			}},
			{ID: domain.ResourceID{Group: "apps", Kind: "Deployment", Name: "api"}, LineHunks: untouched, Changes: []domain.Change{
				{Path: "/spec/replicas", Operation: domain.OperationReplace, Before: 2.0, After: 3.0},
			}},
		},
	}}}
	MaskRepeatedValues(&report, masked)
	app := report.Applications[0]
	if spec := app.SpecChanges[0]; spec.Before != "api" || spec.After != security.Mask || !spec.Redacted {
		t.Fatalf("spec change: %+v", spec)
	}
	cm := app.Resources[0]
	if c := cm.Changes[0]; c.Before != "old-visible" || c.After != security.Mask || !c.Redacted {
		t.Fatalf("copied value: %+v", c)
	}
	if c := cm.Changes[1]; c.Before.(map[string]any)["pass"] != security.Mask || !c.Redacted {
		t.Fatalf("embedded value in a redacted change: %+v", c)
	}
	if c := cm.Changes[2]; c.Redacted || c.After != "b" {
		t.Fatalf("ordinary change masked: %+v", c)
	}
	if lines := cm.LineHunks[0].Lines; lines[0] != "-  copy: old-visible" || !strings.HasPrefix(lines[1], "+  copy: "+security.Mask) {
		t.Fatalf("hunk lines: %q", lines)
	}
	if hunks[0].Lines[1] != "+  copy: fictional-shared-key" {
		t.Fatal("the pass modified shared hunk lines in place")
	}
	if deployment := app.Resources[1]; !reflect.DeepEqual(deployment.LineHunks, untouched) || deployment.Changes[0].Redacted {
		t.Fatalf("unaffected resource changed: %+v", deployment)
	}
}

// Helm inline inputs of a rendered child Application are masked whole as a
// precaution, not because they hold a credential, so their strings do not
// mask the child's resources: in the common App-of-Apps pattern the parent
// passes the image tag the child renders, and the image change must stay
// visible. A Secret payload of the child is still masked where it
// reappears.
func TestMaskRepeatedValuesKeepsValuesFromHelmInlineInputs(t *testing.T) {
	limits := manifest.Limits{Bytes: 1 << 20, Documents: 100}
	parse := func(docs ...string) map[string]manifest.Resource {
		t.Helper()
		resources, err := manifest.Parse(docs, "in-cluster", "demo", limits, nil)
		if err != nil {
			t.Fatal(err)
		}
		return resources
	}
	child := func(tag string) string {
		return `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"name":"api","namespace":"argocd"},"spec":{"source":{"repoURL":"https://git.example.invalid/team/charts.git","path":"api","helm":{` +
			`"values":"image: registry.example.invalid/team/api:` + tag + `\nhost: api-` + tag + `.example.invalid\n",` +
			`"valuesObject":{"image":"registry.example.invalid/team/api:` + tag + `"},` +
			`"parameters":[{"name":"tag","value":"` + tag + `"}]}}}}`
	}
	deployment := func(tag, password string) string {
		return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"demo","labels":{"app.kubernetes.io/version":"` + tag + `"}},"spec":{"template":{"spec":{"containers":[{"name":"api","image":"registry.example.invalid/team/api:` + tag + `","env":[{"name":"HOST","value":"api-` + tag + `.example.invalid"},{"name":"CONN","value":"` + password + `"}]}]}}}}`
	}
	secret := func(password string) string {
		return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"demo"},"stringData":{"conn":"` + password + `"}}`
	}
	masked := &security.MaskedValues{}
	redactor := security.Redactor{Masked: masked}
	parentChanges, _ := manifest.Compare(parse(child("1.4.2")), parse(child("1.4.3")), redactor)
	childChanges, _ := manifest.Compare(
		parse(deployment("1.4.2", "fictional-conn-old"), secret("fictional-conn-old")),
		parse(deployment("1.4.3", "fictional-conn-new"), secret("fictional-conn-new")), redactor)
	report := domain.Report{Applications: []domain.ApplicationResult{{Resources: parentChanges}, {Resources: childChanges}}}
	MaskRepeatedValues(&report, masked)

	var dep domain.ResourceDiff
	for _, resource := range report.Applications[1].Resources {
		if resource.ID.Kind == "Deployment" {
			dep = resource
		}
	}
	shown := map[string]domain.Change{}
	for _, change := range dep.Changes {
		shown[change.Path] = change
	}
	for path, want := range map[string]string{
		"/spec/template/spec/containers/0/image":       "registry.example.invalid/team/api:1.4.3",
		"/spec/template/spec/containers/0/env/0/value": "api-1.4.3.example.invalid",
		"/metadata/labels/app.kubernetes.io~1version":  "1.4.3",
	} {
		if change := shown[path]; change.After != want || change.Redacted {
			t.Errorf("%s: %+v, want %q visible", path, change, want)
		}
	}
	if change := shown["/spec/template/spec/containers/0/env/1/value"]; change.After != security.Mask || !change.Redacted {
		t.Errorf("Secret payload repeated in the Deployment: %+v", change)
	}
	hunks := ""
	for _, hunk := range dep.LineHunks {
		hunks += strings.Join(hunk.Lines, "\n") + "\n"
	}
	if !strings.Contains(hunks, "image: registry.example.invalid/team/api:1.4.3") || strings.Contains(hunks, "fictional-conn-new") {
		t.Errorf("Deployment line hunks:\n%s", hunks)
	}
}

// A value masked only because its key name is sensitive often names a
// reference, such as the Secret key a PushSecret reads; it stays masked in
// place, but other resources that use the same name keep showing it. Only
// Secret payloads and configured paths are repeated.
func TestRepeatedMaskingLeavesKeyNameReferencesVisible(t *testing.T) {
	limits := manifest.Limits{Bytes: 1 << 20, Documents: 100}
	parse := func(docs ...string) map[string]manifest.Resource {
		t.Helper()
		resources, err := manifest.Parse(docs, "in-cluster", "demo", limits, nil)
		if err != nil {
			t.Fatal(err)
		}
		return resources
	}
	push := `{"apiVersion":"external-secrets.io/v1alpha1","kind":"PushSecret","metadata":{"name":"fictional-sa-token","namespace":"demo"},"spec":{"selector":{"secret":{"name":"fictional-sa-token"}},"data":[{"match":{"secretKey":"namespace","remoteRef":{"property":"namespace"}}}]}}`
	account := `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"fictional-sa-token","namespace":"demo"}}`
	masked := &security.MaskedValues{}
	redactor := security.Redactor{Masked: masked}
	changes, _ := manifest.Compare(map[string]manifest.Resource{}, parse(push, account), redactor)
	report := domain.Report{Applications: []domain.ApplicationResult{{Resources: changes}}}
	MaskRepeatedValues(&report, masked)

	shown := ""
	for _, resource := range report.Applications[0].Resources {
		for _, change := range resource.Changes {
			if change.Redacted {
				t.Errorf("%s %s: change marked redacted: %+v", resource.ID.Kind, change.Path, change)
			}
			shown += fmt.Sprint(change.After)
		}
	}
	for _, want := range []string{"fictional-sa-token", "property:namespace"} {
		if !strings.Contains(shown, want) {
			t.Errorf("%q is not shown:\n%s", want, shown)
		}
	}
	if masked.Credentials().Hides("fictional-sa-token") || !masked.Hides("fictional-sa-token") {
		t.Error("a key-name mask must be recorded as a precaution, not as a credential")
	}
}
