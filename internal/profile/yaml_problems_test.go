package profile

import (
	"strings"
	"testing"
)

// A profile error names the line and key of each problem, never a value:
// decoder text quotes values, and a profile can hold tokens.
func TestProfileErrorsNameLineAndKeyWithoutValues(t *testing.T) {
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"unknown field", "version: v1alpha1\nrender:\n  maxConcurency: 4\n", "line 3: unknown field maxConcurency"},
		{"flag-style key", "version: v1alpha1\nrender:\n  max-concurrency: 4\n", "line 3: unknown field max-concurrency"},
		{"duplicate key", "version: v1alpha1\nversion: v1alpha1\n", "line 2: key version is already defined at line 1"},
		{"wrong kind", "version: v1alpha1\nrender:\n  maxConcurrency: ghp_fictionalsecretvalue\n", "line 3: a string is not valid here; expected a number"},
		{"scalar for a section", "version: v1alpha1\ngit: fictional-secret-scalar\n", "line 2: a string is not valid here; expected a mapping"},
		// yaml.v3 counts parser error lines from zero; the error names the
		// line in the file.
		{"syntax", "version: v1alpha1\nrender:\n  maxConcurrency: [1\n", "line 3: did not find expected ',' or ']'"},
		{"missing key", "version: v1alpha1\nrender:\n  maxConcurrency: 1\n maxDepth: 2\n", "line 4: did not find expected key"},
		{"undefined tag", "version: v1alpha1\nname: !fictional!x 1\n", "line 2: found undefined tag handle"},
		{"scanner error", "version: v1alpha1\nrender:\n  maxConcurrency: 1: 2\n", "line 3: mapping values are not allowed in this context"},
		{"tab", "version: v1alpha1\n\trender: {}\n", "line 2: found a tab character that violates indentation"},
		{"credential-like key", "version: v1alpha1\nghp_0123456789abcdefFICTIONAL: x\n", "unknown field (name withheld)"},
		{"digit-heavy key", "version: v1alpha1\nabc1234567890: x\n", "unknown field (name withheld)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v does not contain %q", err, tc.want)
			}
			for _, secret := range []string{"ghp_", "fictional", "abc1234567890"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error repeats document text %q: %v", secret, err)
				}
			}
		})
	}
}

func TestProfileErrorsAreBounded(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("version: v1alpha1\n")
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		doc.WriteString("unknown" + name + ": 1\n")
	}
	_, err := Parse([]byte(doc.String()))
	if err == nil || strings.Count(err.Error(), "unknown field") != maxYAMLProblems || !strings.HasSuffix(err.Error(), "more problems follow") {
		t.Fatalf("problems not bounded: %v", err)
	}
}
