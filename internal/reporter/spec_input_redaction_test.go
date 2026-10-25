package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// referenceChanges returns an added ExternalSecret data item, whose
// secretKey is a reference only at its own path, and a literal value at a
// sensitive path, which only its own path shows to be sensitive. tag makes
// the values distinct per report section.
func referenceChanges(tag string) []domain.Change {
	return []domain.Change{
		{Path: "/spec/data/1", Operation: "add", After: map[string]any{"secretKey": tag + "-secret-key", "remoteRef": map[string]any{"key": "app", "property": "slack"}}},
		{Path: "/spec/apiToken", Operation: "replace", Before: "fictional-" + tag + "-old", After: "fictional-" + tag + "-new"},
	}
}

// The final report pass redacts Application spec changes and value file
// changes at their own paths, as it does resource changes: a
// reference stays visible and a literal value at a sensitive path stays
// masked in every format.
func TestSpecAndValueFileChangesAreRedactedAtTheirOwnPath(t *testing.T) {
	r := tailChangeReport("a", "b")
	app := &r.Applications[0]
	app.SpecChanges = referenceChanges("spec")
	app.InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.example.test/org/charts", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete", Changes: referenceChanges("input")}}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"spec-secret-key", "input-secret-key"} {
			if !strings.Contains(out, `"secretKey":"`+want+`"`) && !strings.Contains(out, `"secretKey": "`+want+`"`) && !strings.Contains(out, "&#34;secretKey&#34;:&#34;"+want+"&#34;") && !strings.Contains(out, "+secretKey: "+want+"\n") {
				t.Fatalf("%s hid reference %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "fictional-") {
			t.Fatalf("%s exposed a literal value:\n%s", format, out)
		}
	}
}
