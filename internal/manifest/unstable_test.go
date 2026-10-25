package manifest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// A field marked as differing between two renders of identical inputs is not
// compared: it is listed in unknownPaths, the comparison is incomplete, and
// the other fields are still compared.
func TestUnstableFieldsAreNotCompared(t *testing.T) {
	u := `"` + domain.RenderUnstable + `"`
	render := func(env, configured, extra string) map[string]Resource {
		docs := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"password":` + u + `,"user":"YXBw"}}
---
{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"template":{"metadata":{"annotations":{"checksum":` + u + `}},"spec":{"containers":[{"name":"app","env":[{"name":"MODE","value":"` + env + `"}]}]}}}}
---
{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"generated"},"data":{"key":` + configured + `}}
---
{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"plain"},"data":{"key":"same"}}
` + extra
		return parsed(t, docs)
	}
	diff, complete := Compare(render("a", u, ""), render("b", `"fixed"`, `---
{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new"},"data":{"key":`+u+`}}
`), security.Redactor{})
	if complete {
		t.Fatal("a comparison with fields that differ between renders was reported complete")
	}
	byName := map[string]domain.ResourceDiff{}
	for _, d := range diff {
		byName[d.ID.Name] = d
	}
	for _, tc := range []struct {
		name, state string
		unknown     []string
		changes     []string
	}{
		{"db", "unknown", []string{"/data/password"}, nil},
		{"api", "changed", []string{"/spec/template/metadata/annotations/checksum"}, []string{"/spec/template/spec/containers/0/env/0/value"}},
		{"generated", "unknown", []string{"/data/key"}, nil},
		{"new", "added", []string{"/data/key"}, []string{"/"}},
	} {
		d := byName[tc.name]
		paths := []string{}
		for _, c := range d.Changes {
			paths = append(paths, c.Path)
		}
		if string(d.State) != tc.state || d.Comparison != "incomplete" || d.IncompleteReason != domain.IncompleteUnstableFields || !slices.Equal(d.UnknownPaths, tc.unknown) || !slices.Equal(paths, tc.changes) && !(len(paths) == 0 && tc.changes == nil) {
			t.Errorf("%s: state %s, comparison %s, unknownPaths %v, changes %v; want %s, incomplete, %v, %v", tc.name, d.State, d.Comparison, d.UnknownPaths, paths, tc.state, tc.unknown, tc.changes)
		}
	}
	if plain := byName["plain"]; plain.State != "unchanged" || plain.Comparison != "complete" || plain.UnknownPaths != nil {
		t.Errorf("a resource without such fields was affected: %+v", plain)
	}
	encoded, _ := json.Marshal(diff)
	if strings.Contains(string(encoded), "YXBw") {
		t.Fatalf("a Secret value leaked: %s", encoded)
	}
}
