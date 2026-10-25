package reporter

import (
	"bytes"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// A report written in a primary format and as --json-output is redacted
// once; every output must equal what a separate Encode call writes, stay
// masked, and an unknown format must fail with the same message.
func TestRedactedEncodesEveryFormatLikeSeparateEncodeCalls(t *testing.T) {
	r := inputChangeReport()
	r.Applications[0].InputChanges = []domain.InputChange{{Kind: "inlineValues", Path: "/spec/source/helm/values", State: "changed", Comparison: "complete", Changes: []domain.Change{
		{Path: "/image/tag", Operation: "replace", Before: "fictional-v1", After: "fictional-v2"},
	}}}
	r.Applications[0].Issues = []domain.Issue{{Code: "render_failed", Stage: "render", Reason: "token fictional-secret-token rejected", Action: "rotate it"}}
	redactor := security.Redactor{Secrets: []string{"fictional-secret-token"}}
	for _, primary := range Formats {
		redacted, err := Redact(r, redactor)
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{primary, "json", primary} {
			paired, err := redacted.Encode(format)
			if err != nil {
				t.Fatal(err)
			}
			separate, err := Encode(r, format, redactor)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(paired, separate) {
				t.Fatalf("%s after %s differs from a separate Encode call", format, primary)
			}
			if out := string(paired); strings.Contains(out, "fictional-") {
				t.Fatalf("%s after %s exposed a masked value:\n%s", format, primary, out)
			}
		}
		// Two callers appending to their outputs must not write into each
		// other's memory.
		first, _ := redacted.Encode("json")
		second, _ := redacted.Encode("json")
		first, second = append(first, 'a'), append(second, 'b')
		if first[len(first)-1] != 'a' || second[len(second)-1] != 'b' {
			t.Fatal("json outputs share writable memory")
		}
	}
	redacted, err := Redact(r, redactor)
	if err != nil {
		t.Fatal(err)
	}
	_, pairedErr := redacted.Encode("html")
	_, separateErr := Encode(r, "html", redactor)
	if pairedErr == nil || separateErr == nil || pairedErr.Error() != separateErr.Error() {
		t.Fatalf("unknown format errors differ: %v / %v", pairedErr, separateErr)
	}
}
