package reporter

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"diffaro/internal/domain"
)

// Display of values and before/after pairs.

// displayJSON encodes a value for display. Unlike json.Marshal it keeps &, <
// and > as written, so a Traefik match rule or a PromQL comparison reads as in
// the manifest; Markdown formats still escape HTML afterwards.
func displayJSON(value any) []byte {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// short bounds a JSON value to 4000 bytes, cutting at a rune boundary.
func short(b []byte) string {
	s := string(b)
	if len(s) > 4000 {
		cut := 4000
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "… [truncated; use JSON]"
	}
	return esc(s)
}

// Each format bounds a long pair at its own width; a pair is long when either
// side is past that width (domain.RunesOver), or at it too where the format
// says so (domain.RunesAtLeast). Both sides of a long pair then start shortly
// before their first difference (domain.PairFrom), as in line hunks.

// shortPair bounds a before/after pair of JSON values like short. When either
// side is longer than 4000 bytes, both start near their first difference.
func shortPair(before, after []byte) (string, string) {
	b, a := domain.PairFrom(string(before), string(after), len(before) > 4000 || len(after) > 4000, 1000)
	return short([]byte(b)), short([]byte(a))
}

// prPair bounds a before/after pair like prText. When either side is longer
// than max, both start near their first difference.
func prPair(before, after string, max int) (string, string) {
	before, after = domain.PairFrom(before, after, domain.RunesOver(before, after, max), max/4)
	return prText(before, max), prText(after, max)
}
