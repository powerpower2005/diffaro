package manifest

import (
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"

	"go.yaml.in/yaml/v3"
)

// MaskHunkLines returns a copy of line hunks in which every line whose
// string value masked reports hidden shows security.Mask instead of the
// value, and whether it masked any. A changed line
// masked this way is marked as a hidden change, as line hunks mark
// redacted changes. Keys, list markers and other values stay.
func MaskHunkLines(hunks []domain.LineHunk, masked *security.MaskedValues) ([]domain.LineHunk, bool) {
	if len(hunks) == 0 {
		return hunks, false
	}
	out := make([]domain.LineHunk, len(hunks))
	masking := false
	for i, hunk := range hunks {
		out[i] = hunk
		out[i].Lines = append([]string(nil), hunk.Lines...)
		for j, line := range hunk.Lines {
			if text, hit := maskLine(line, masked); hit {
				out[i].Lines[j], masking = text, true
			}
		}
	}
	return out, masking
}

// maskLine masks the value of one hunk line: a mark (' ', '-' or '+')
// followed by a YAML line, such as "+  key: value", "-  - value" or a line of
// a block scalar. The value is tried as the whole line and as the scalar
// after list markers and a key. Only a value YAML reads as a string counts:
// masked values are strings, and change values keep numbers and booleans.
func maskLine(line string, masked *security.MaskedValues) (string, bool) {
	if line == "" || strings.Contains(line, security.Mask) {
		return line, false
	}
	mark, body := line[:1], line[1:]
	trimmed := strings.TrimLeft(body, " ")
	indent := body[:len(body)-len(trimmed)]
	// A line cut to its display width ends with an ellipsis.
	trimmed = strings.TrimSuffix(trimmed, "…")
	note := ""
	if mark == "-" || mark == "+" {
		note = hiddenNote
	}
	if hidesScalar(trimmed, masked) {
		return mark + indent + security.Mask + note, true
	}
	prefix, rest := "", trimmed
	for strings.HasPrefix(rest, "- ") {
		prefix, rest = prefix+"- ", rest[2:]
	}
	if !strings.HasPrefix(rest, `"`) && !strings.HasPrefix(rest, "'") {
		if key, value, ok := strings.Cut(rest, ": "); ok {
			prefix, rest = prefix+key+": ", value
		}
	}
	if rest != trimmed && hidesScalar(rest, masked) {
		return mark + indent + prefix + security.Mask + note, true
	}
	return line, false
}

// hidesScalar reports whether text, read as one YAML scalar, is a string
// that masked hides. Text YAML cannot read or reads as nothing, such as a
// block scalar line or a comment, is compared as written. Text that reads as a mapping or list is not: its key
// or marker stays, and maskLine checks the value after it.
func hidesScalar(text string, masked *security.MaskedValues) bool {
	if text == "" {
		return false
	}
	var node yaml.Node
	if yaml.Unmarshal([]byte(text), &node) != nil || len(node.Content) == 0 {
		return masked.Hides(text)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.ScalarNode {
		return false
	}
	scalar := node.Content[0]
	if scalar.Tag != "!!str" {
		return false
	}
	return masked.Hides(scalar.Value)
}
