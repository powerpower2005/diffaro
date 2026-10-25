package localhelm

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// maxChartMessage bounds a chart message shown in a report.
const maxChartMessage = 300

var (
	// failMessage is the message of a fail or required call: the rest of
	// the line after Helm's "execution error at (FILE:LINE:COL): ".
	failMessage = regexp.MustCompile(`execution error at \(([A-Za-z0-9_./-]+):([0-9]+):[0-9]+\): ([^\n]*)`)
	// stringLiteral is a Go template string: interpreted or raw.
	stringLiteral = regexp.MustCompile("\"(?:[^\"\\\\\\n]|\\\\.)*\"|`[^`]*`")
)

// chartMessage returns the message of a failed fail or required call when
// it is a string literal on the failing line of the template source, as in
// required "image.tag is required" .Values.image.tag. Such a message is
// part of the chart in Git and holds no value. A message built from values,
// as by printf, matches no literal and stays withheld.
func chartMessage(ch *chart.Chart, raw string) (string, bool) {
	m := failMessage.FindStringSubmatch(raw)
	if m == nil || ch == nil {
		return "", false
	}
	message := strings.TrimSpace(m[3])
	if message == "" || len(message) > maxChartMessage || !utf8.ValidString(message) {
		return "", false
	}
	source := templateSource(ch, m[1])
	n, err := strconv.Atoi(m[2])
	if source == nil || err != nil || n < 1 {
		return "", false
	}
	lines := strings.Split(string(source), "\n")
	if n > len(lines) {
		return "", false
	}
	for _, literal := range stringLiteral.FindAllString(lines[n-1], -1) {
		if text, err := strconv.Unquote(literal); err == nil && text == message {
			return message, true
		}
	}
	return "", false
}

// templateSource is the source of the template Helm names
// CHART/templates/FILE or CHART/charts/SUBCHART/templates/FILE, or nil.
func templateSource(ch *chart.Chart, name string) []byte {
	rest, ok := strings.CutPrefix(name, ch.Name()+"/")
	if !ok {
		return nil
	}
	for _, file := range ch.Templates {
		if file.Name == rest {
			return file.Data
		}
	}
	if rest, ok = strings.CutPrefix(rest, "charts/"); ok {
		for _, dependency := range ch.Dependencies() {
			if source := templateSource(dependency, rest); source != nil {
				return source
			}
		}
	}
	return nil
}
