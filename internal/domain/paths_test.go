package domain

import "testing"

// Repository and chart paths must stay inside their root; only the colon
// rule differs between the two predicates.
func TestSafeRelativePathRules(t *testing.T) {
	for _, tc := range []struct {
		path        string
		strict, any bool
	}{
		{"charts/api/values.yaml", true, true},
		{".", true, true},
		{"a..b/c", true, true},
		{"charts/api:v1/values.yaml", false, true},
		{"", false, false},
		{"..", false, false},
		{"../outside", false, false},
		{"/etc/passwd", false, false},
		{"charts//api", false, false},
		{"charts/./api", false, false},
		{"charts/api/", false, false},
		{"charts/../api", false, false},
		{`charts\api`, false, false},
		{"charts/\x00api", false, false},
	} {
		if got := SafeRelativePath(tc.path); got != tc.strict {
			t.Errorf("SafeRelativePath(%q) = %t, want %t", tc.path, got, tc.strict)
		}
		if got := SafeRelativePathAllowingColon(tc.path); got != tc.any {
			t.Errorf("SafeRelativePathAllowingColon(%q) = %t, want %t", tc.path, got, tc.any)
		}
	}
}
