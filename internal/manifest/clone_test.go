package manifest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestCompareResultsDoNotAliasInputs(t *testing.T) {
	base := parsed(t, deployment+"  template:\n    spec:\n      containers:\n      - name: api\n        image: example/api:1\n")
	head := parsed(t, deployment+"  template:\n    spec:\n      containers:\n      - name: api\n        image: example/api:2\n")
	same := parsed(t, deployment)
	diff, _ := Compare(base, head, security.Redactor{})
	unchanged, _ := Compare(same, same, security.Redactor{})
	before, _ := json.Marshal(append(diff, unchanged...))
	for _, side := range []map[string]Resource{base, head, same} {
		for _, r := range side {
			spec := r.Object["spec"].(map[string]any)
			spec["replicas"] = "mutated"
			if template, ok := spec["template"].(map[string]any); ok {
				template["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "mutated"
			}
		}
	}
	after, _ := json.Marshal(append(diff, unchanged...))
	if string(before) != string(after) || diff[0].State != "changed" || unchanged[0].State != "unchanged" || len(unchanged[0].Changes) != 0 {
		t.Fatalf("comparison result changed after input mutation:\n%s\n%s", before, after)
	}
}

func TestCompareLeavesInputsAndSecretMaskingIntact(t *testing.T) {
	secret := func(value string) map[string]Resource {
		return parsed(t, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: credentials\n  annotations:\n    note: fictional-annotation-"+value+"\ndata:\n  key: fictional-payload-"+value+"\n")
	}
	base, head := secret("a"), secret("b")
	original := domain.CloneSpec(base[firstKey(base)].Object)
	diff, complete := Compare(base, head, security.Redactor{})
	encoded, _ := json.Marshal(diff)
	// Readable payloads with different values are a masked change.
	if !complete || diff[0].State != "changed" || strings.Contains(string(encoded), "fictional-") {
		t.Fatalf("Secret masking changed: %s", encoded)
	}
	if !reflect.DeepEqual(base[firstKey(base)].Object, original) {
		t.Fatal("Compare mutated its input while masking a Secret")
	}
	equal, _ := Compare(base, base, security.Redactor{})
	if equal[0].State != "unchanged" || equal[0].Comparison != "complete" {
		t.Fatal("equal readable Secrets must compare as unchanged")
	}
}

// Compare walks non-Secret objects without copying them, so it must leave
// them unchanged, including named lists, sensitive env vars and configured
// paths, which are redacted in the reported values.
func TestCompareLeavesNonSecretInputsIntact(t *testing.T) {
	container := func(image, token string) string {
		return "      - name: api\n        image: " + image + "\n        env:\n        - name: API_TOKEN\n          value: " + token + "\n        - name: LEVEL\n          value: info\n"
	}
	base := parsed(t, deployment+"  custom: fictional-a\n  template:\n    spec:\n      containers:\n"+container("example/api:1", "fictional-a"))
	head := parsed(t, deployment+"  custom: fictional-b\n  template:\n    spec:\n      containers:\n"+container("example/api:2", "fictional-b")+"      - name: sidecar\n        image: example/sidecar:1\n")
	originals := map[string]any{}
	for name, side := range map[string]map[string]Resource{"base": base, "head": head} {
		originals[name] = domain.CloneSpec(side[firstKey(side)].Object)
	}
	diff, _ := Compare(base, head, security.Redactor{Paths: []string{"/spec/custom"}})
	if diff[0].State != "changed" || len(diff[0].LineHunks) == 0 {
		t.Fatalf("expected a changed Deployment with line hunks: %+v", diff[0])
	}
	for name, side := range map[string]map[string]Resource{"base": base, "head": head} {
		if !reflect.DeepEqual(side[firstKey(side)].Object, originals[name]) {
			t.Fatalf("Compare mutated the %s object", name)
		}
	}
}

func TestParseCloneMatchesCloneSpec(t *testing.T) {
	raw := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: numbers\n  uid: dropped\ndata:\n  big: \"12345678901234567890\"\nspec:\n  float: 1.50\n  int: 12345678901234567890\n  list: [3, 1, 2]\n  html: \"<a&b>\"\n"
	got := parsed(t, raw)[firstKey(parsed(t, raw))].Object
	object := domain.DecodeSpec([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"numbers","namespace":"demo"},"data":{"big":"12345678901234567890"},"spec":{"float":1.5,"int":12345678901234567890,"list":[3,1,2],"html":"<a&b>"}}`))
	want := domain.CloneSpec(object)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed object differs from CloneSpec normalization:\n%#v\n%#v", got, want)
	}
}

func firstKey(m map[string]Resource) string {
	for k := range m {
		return k
	}
	return ""
}
