package manifest

import (
	"reflect"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// A line hunk value that a run masked elsewhere is masked with the rule of
// value file changes: equal, or from 8 bytes contained in a masked string.
// Keys and list markers stay, changed lines are marked hidden, and numbers,
// booleans and other values are kept. A "key: value"
// or list line that appears whole inside a masked string keeps its key or
// marker; only a line YAML cannot read or reads as nothing, such as a block
// scalar line or a comment, is masked whole.
func TestMaskHunkLinesMasksRepeatedValues(t *testing.T) {
	masked := &security.MaskedValues{}
	masked.Add([]any{"fictional-key-a", "fictional-key-b", "postgres://u:fictional-embedded@db/app", "fictional-block-1\nfictional-block-2\n", "12345678", "short",
		"config:\n  image: registry.example.invalid/team/api:1.4.2\n  tags:\n  - fictional-list-item\n  note: fictional: block: line\n  # fictional comment line\n"})
	hunks := []domain.LineHunk{{Path: "/data", BaseStart: 1, HeadStart: 1, Lines: []string{
		" data:",
		"-  copy: fictional-key-a",
		"+  copy: fictional-key-b",
		"   quoted: \"fictional-key-a\"",
		"+  - fictional-key-b",
		"+  - name: fictional-key-b",
		"     fictional-block-2",
		"   pass: fictional-embedded",
		"   port: 12345678",
		"   port-text: \"12345678\"",
		"   user: short",
		"   other: shorter-than-masked",
		"   note: value with fictional-key-a inside",
		"+  secret: [REDACTED]  # value changed (hidden)",
		"-  image: registry.example.invalid/team/api:1.4.2",
		"   - fictional-list-item",
		"     fictional: block: line",
		"     # fictional comment line",
		"   …",
	}}}
	original := append([]string(nil), hunks[0].Lines...)
	got, hit := MaskHunkLines(hunks, masked)
	want := []string{
		" data:",
		"-  copy: [REDACTED]" + hiddenNote,
		"+  copy: [REDACTED]" + hiddenNote,
		"   quoted: [REDACTED]",
		"+  - [REDACTED]" + hiddenNote,
		"+  - name: [REDACTED]" + hiddenNote,
		"     [REDACTED]",
		"   pass: [REDACTED]",
		"   port: 12345678",
		"   port-text: [REDACTED]",
		"   user: [REDACTED]",
		"   other: shorter-than-masked",
		"   note: value with fictional-key-a inside",
		"+  secret: [REDACTED]  # value changed (hidden)",
		"-  image: [REDACTED]" + hiddenNote,
		"   - [REDACTED]",
		"     [REDACTED]",
		"     [REDACTED]",
		"   …",
	}
	if !hit || !reflect.DeepEqual(got[0].Lines, want) {
		t.Fatalf("masked lines:\n%q\nwant\n%q", got[0].Lines, want)
	}
	if !reflect.DeepEqual(hunks[0].Lines, original) {
		t.Fatal("the input hunks were modified")
	}
	if _, hit := MaskHunkLines([]domain.LineHunk{{Lines: []string{" a: b", "+c: visible-value"}}}, masked); hit {
		t.Fatal("hunks without masked values reported masking")
	}
}
