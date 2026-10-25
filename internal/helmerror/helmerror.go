// Package helmerror reads safe facts from Helm error text: the failing
// template file and line, a fixed failure category and the include call
// site. It never returns message text, which can quote values. Local Helm,
// repo-server and Argo CD Application API failures share it so all name a
// render failure alike.
package helmerror

import (
	"fmt"
	"regexp"
	"strings"

	"diffaro/internal/domain"
)

// Template locations come from the chart source, never from values. Only the
// reported file:line and a fixed category are kept; the raw message can quote
// values (fail/required text, printed fields) and is always withheld.
var location = regexp.MustCompile(`(?:^|template: |parse error at \(|execution error at \()([A-Za-z0-9_./-]+):([0-9]+)(?::[0-9]+)?(?:\)|:|\n)`)

var categories = []struct{ marker, category string }{
	{"execution error at", "the chart stopped rendering with fail or required"},
	{"error calling fail", "the chart stopped rendering with fail or required"},
	{"error calling required", "the chart stopped rendering with fail or required"},
	{"nil pointer evaluating", "a nil value was referenced"},
	{"map has no entry for key", "a map key is missing"},
	{"wrong type for value", "a value has the wrong type"},
	{"can't evaluate field", "a field cannot be evaluated"},
	{"wrong number of args", "a function received the wrong number of arguments"},
	{"parse error", "template syntax is invalid"},
	{"on zero Value", "a function received a null value"},
}

// frame matches one frame of a Go template execution error from its
// start: the template file and line, then the called function. The chain
// continues only through include, whose error is the included template's
// frame; any other function or shape ends it, so a message that can quote
// values, such as that of fail or tpl, is never parsed.
var frame = regexp.MustCompile(`^template: ([A-Za-z0-9_./-]+):([0-9]+):[0-9]+: executing "[^"\n]*" at <[^\n]*?>: (?:error calling ([A-Za-z0-9_]+): )?`)

// callSite names the innermost template file, line and called
// function of an include chain. Only file paths, line numbers and
// function identifiers are kept; template actions and messages are not.
func callSite(raw, file, line string) string {
	start := strings.Index(raw, "template: ")
	if start < 0 {
		return ""
	}
	rest := raw[start:]
	var innerFile, innerLine, function string
	for {
		m := frame.FindStringSubmatch(rest)
		if m == nil || !domain.SafeRelativePath(m[1]) || !strings.Contains(m[1], "/templates/") || len(m[2]) > 7 {
			break
		}
		if innerFile == "" && (m[1] != file || m[2] != line) {
			return ""
		}
		innerFile, innerLine, function = m[1], m[2], m[3]
		rest = rest[len(m[0]):]
		// Only include is followed by the frame of the included template;
		// other functions are followed by their message.
		if function != "include" {
			break
		}
	}
	switch {
	case innerFile == "":
		return ""
	case innerFile == file && innerLine == line && function == "":
		return ""
	case innerFile == file && innerLine == line:
		return " (calling " + function + ")"
	case function == "":
		return " (at " + innerFile + ":" + innerLine + ")"
	}
	return " (at " + innerFile + ":" + innerLine + " calling " + function + ")"
}

// Location returns the first template file and line of a Helm error. The
// file is a clean chart-relative path under templates/. The first location
// precedes any message text, so values in the message are never read as one.
func Location(raw string) (file, line string, ok bool) {
	m := location.FindStringSubmatch(raw)
	if len(m) != 3 || !domain.SafeRelativePath(m[1]) || !strings.Contains(m[1], "/templates/") || len(m[2]) > 7 {
		return "", "", false
	}
	return m[1], m[2], true
}

// Describe names a Helm template failure as "template FILE:LINE failed:
// CATEGORY", with the include call site when one is safe. It
// reports false when the error has no safe template location.
func Describe(raw string) (string, bool) {
	file, line, ok := Location(raw)
	if !ok {
		return "", false
	}
	category := "template execution failed"
	for _, c := range categories {
		if strings.Contains(raw, c.marker) {
			category = c.category
			break
		}
	}
	return fmt.Sprintf("template %s:%s failed: %s%s", file, line, category, callSite(raw, file, line)), true
}

// ReproduceHint is the action that shows a withheld Helm message to an
// operator who may read the Application's values.
const ReproduceHint = "to see the withheld message, run helm template on the chart at this revision with the Application's values"
