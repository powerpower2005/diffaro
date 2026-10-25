package manifest

import (
	"strings"
	"testing"
)

// g53Nested returns a values document with levels nested mappings.
func g53Nested(levels int) []byte {
	var b strings.Builder
	for i := range levels {
		b.WriteString(strings.Repeat("  ", i) + "k:\n")
	}
	b.WriteString(strings.Repeat("  ", levels) + "leaf: value\n")
	return []byte(b.String())
}

// Unsupported YAML, including nesting deeper than 64 levels,
// leaves the comparison incomplete instead of being parsed.
func TestParseValuesRejectsExcessiveNesting(t *testing.T) {
	if _, err := ParseValues(g53Nested(32)); err != nil {
		t.Fatal("moderate nesting must parse", err)
	}
	values, err := ParseValues(g53Nested(80))
	if err == nil || values != nil || !strings.Contains(err.Error(), "excessive nesting") {
		t.Fatalf("nesting deeper than 64 levels must be rejected: %v", err)
	}
}
