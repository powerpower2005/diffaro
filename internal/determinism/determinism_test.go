package determinism

import (
	"diffaro/internal/domain"
	"strings"
	"testing"
)

func TestCompareIgnoresDocumentOrderOnly(t *testing.T) {
	const a = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: demo}\ndata: {k: v}\n"
	const b = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b, namespace: demo}\ndata: {k: v}\n"
	const a2 = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: demo}\ndata: {k: other}\n"
	for _, tc := range []struct {
		name          string
		first, second string
		want          bool
	}{
		{"identical", a + "---\n" + b, a + "---\n" + b, true},
		{"reordered documents", a + "---\n" + b, b + "---\n" + a, true},
		{"comments and formatting", "# first\n" + a + "---\n" + b, b + "---\n# second\n" + a, true},
		{"different content", a + "---\n" + b, a2 + "---\n" + b, false},
		{"missing document", a + "---\n" + b, a, false},
		// The last occurrence of a repeated identity wins, so its order matters.
		{"repeated identity reordered", a + "---\n" + a2, a2 + "---\n" + a, false},
		{"repeated identity kept", a + "---\n" + a2 + "---\n" + b, b + "---\n" + a + "---\n" + a2, true},
		{"invalid YAML", "a: [", "a: [ ", false},
	} {
		if _, got, _ := Compare(tc.first, tc.second); got != tc.want {
			t.Errorf("%s: same = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// Argo CD reads manifests with sigs.k8s.io/yaml, where a repeated mapping key
// keeps its last value; the determinism check must not reject such output.
func TestRepeatedMappingKeysKeepLastValueLikeArgoCD(t *testing.T) {
	const doc = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n  annotations:\n    image-tag: old\n    other: x\n    image-tag: new\ndata: {k: v}\n"
	if _, same, _ := Compare(doc, strings.Replace(doc, "data: {k: v}", "data:\n  k: v", 1)); !same {
		t.Fatal("determinism check rejected repeated keys")
	}
}

// Two renders of identical inputs that differ only in some field values keep
// every equal field and mark each differing one; renders that describe
// different resources are not paired.
func TestCompareMarksDifferingFieldsAndKeepsEqualOnes(t *testing.T) {
	render := func(password, suffix string) string {
		return "apiVersion: v1\nkind: Secret\nmetadata: {name: db" + suffix + "}\ndata: {password: " + password + ", user: app}\n---\n" +
			"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\nspec:\n  replicas: 3\n  template:\n    metadata:\n      annotations: {checksum: " + password + "}\n    spec:\n      containers: [{name: app, image: app:v1}]\n"
	}
	marked, ok := markOnly(render("r1", ""), render("r2", ""))
	if !ok {
		t.Fatal("renders of the same resources were not paired")
	}
	objects, ok := targetDocuments(marked)
	if !ok || len(objects) != 2 {
		t.Fatalf("marked output is not two documents: %s", marked)
	}
	for _, want := range []string{
		`"data":{"password":"` + domain.RenderUnstable + `","user":"app"}`,
		`"annotations":{"checksum":"` + domain.RenderUnstable + `"}`,
		`"replicas":3`,
		`"image":"app:v1"`,
	} {
		if !strings.Contains(marked, want) {
			t.Fatalf("marked output lacks %s:\n%s", want, marked)
		}
	}
	if strings.Contains(marked, "r1") || strings.Contains(marked, "r2") {
		t.Fatalf("a differing value was kept:\n%s", marked)
	}
	// Document order alone is not a difference.
	first := render("r1", "")
	parts := strings.SplitN(first, "---\n", 2)
	if again, ok := markOnly(parts[1]+"---\n"+parts[0], render("r2", "")); !ok || again != marked {
		t.Fatalf("reordered documents were marked differently:\n%s", again)
	}
	if _, ok := markOnly(render("r1", "-a"), render("r1", "-b")); ok {
		t.Fatal("renders with differently named resources were paired")
	}
	if _, ok := markOnly(render("r1", ""), render("r1", "")+"---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n"); ok {
		t.Fatal("renders with an extra resource were paired")
	}
}

// markOnly is the marked output of Compare for renders that differ.
func markOnly(first, second string) (string, bool) {
	marked, same, ok := Compare(first, second)
	return marked, !same && ok
}

// Compare parses each render once, also when the renders differ and their
// differing fields are marked.
func TestCompareParsesEachRenderOnce(t *testing.T) {
	calls := 0
	parse = func(raw string) (map[string][]string, bool) {
		calls++
		return targetDocuments(raw)
	}
	defer func() { parse = targetDocuments }()
	render := func(password string) string {
		return "apiVersion: v1\nkind: Secret\nmetadata: {name: db}\ndata: {password: " + password + "}\n"
	}
	for _, tc := range []struct {
		name          string
		first, second string
		same, ok      bool
		parses        int
	}{
		{"byte-identical", render("r1"), render("r1"), true, true, 0},
		{"same targets", render("r1"), "# reformatted\n" + render("r1"), true, true, 2},
		{"differing field", render("r1"), render("r2"), false, true, 2},
	} {
		calls = 0
		marked, same, ok := Compare(tc.first, tc.second)
		if same != tc.same || ok != tc.ok || calls != tc.parses {
			t.Errorf("%s: same=%t ok=%t parses=%d, want same=%t ok=%t parses=%d", tc.name, same, ok, calls, tc.same, tc.ok, tc.parses)
		}
		if !same && !strings.Contains(marked, domain.RenderUnstable) {
			t.Errorf("%s: differing field not marked: %s", tc.name, marked)
		}
	}
}
