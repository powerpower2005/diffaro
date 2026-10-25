package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func encodeText(t *testing.T, r domain.Report, format string) string {
	t.Helper()
	data, err := Encode(r, format, security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func lineWith(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// A value hidden on both sides reads as changed wherever it is
// shown: an input key change and a desired Application spec change, in PR
// and full Markdown alike.
func TestHiddenInputAndSpecChangesReadAsChanged(t *testing.T) {
	r := tailChangeReport("a", "b")
	app := &r.Applications[0]
	app.SpecChanges = []domain.Change{{Path: "/source/helm/parameters/0/value", Operation: "replace", Before: security.Mask, After: security.Mask, Redacted: true}}
	app.InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/secrets.yaml", State: "changed", Comparison: "complete",
		Changes: []domain.Change{{Path: "/auth/password", Operation: "replace", Before: security.Mask, After: security.Mask, Redacted: true}}}}
	for _, tc := range []struct{ format, path string }{
		{"pr-markdown", "`/auth/password`"},
		{"markdown", "`/auth/password`"},
		{"pr-markdown", "@@ /source/helm/parameters/0/value"},
		{"markdown", "`/source/helm/parameters/0/value`"},
	} {
		out := encodeText(t, r, tc.format)
		if strings.HasPrefix(tc.path, "@@ ") {
			// PR diff blocks mark the masked pair as changed.
			if !strings.Contains(out, tc.path+" @@\n-'[REDACTED]'  # "+hiddenText+"\n+'[REDACTED]'  # "+hiddenText+"\n") {
				t.Fatalf("%s does not show %s as a hidden change:\n%s", tc.format, tc.path, out)
			}
			continue
		}
		line := lineWith(out, tc.path)
		if line == "" {
			t.Fatalf("%s does not show %s:\n%s", tc.format, tc.path, out)
		}
		if !strings.Contains(line, hiddenText) || strings.Contains(line, security.Mask) {
			t.Fatalf("%s shows %s as %q, want %q without the mask", tc.format, tc.path, line, hiddenText)
		}
	}
}
