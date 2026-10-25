package reporter

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
)

// Escaping of untrusted text for Markdown, code spans and fenced diff
// blocks. The terminal escapes in terminalText.

func esc(s string) string {
	var visible strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' || r == '\t' {
			fmt.Fprintf(&visible, "\\u%04x", r)
		} else {
			visible.WriteRune(r)
		}
	}
	s = visible.String()
	return markdownEscapes.Replace(html.EscapeString(s))
}

// markdownEscapes is built once: strings.Replacer is safe for concurrent use
// and building it on each call dominated report allocations.
var markdownEscapes = strings.NewReplacer("`", "&#96;", "\n", " ", "\r", " ", "|", "&#124;", "*", "&#42;", "[", "&#91;", "]", "&#93;", "\\", "&#92;")

// prText bounds a value to max runes, marking a cut with "…", and escapes it.
func prText(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		value = string(runes[:max]) + "…"
	}
	return esc(value)
}

// codeSpan matches one inline code span. esc encodes every backtick in
// content, so the backticks on a line pair up as span delimiters.
var codeSpan = regexp.MustCompile("`[^`\n]*`")

// htmlCodeSpans turns code spans whose escaped content contains an HTML entity
// into <code> elements. Markdown prints code span content literally, so GitHub
// would show &#34; instead of a quote. Inside <code> the entity is decoded,
// and the escaping still keeps table cells and emphasis intact. An empty
// value, which Markdown would print as two backticks, reads "(none)".
func htmlCodeSpans(markdown string) string {
	lines := strings.SplitAfter(markdown, "\n")
	fence := ""
	for i, line := range lines {
		var literal bool
		if fence, literal = textFence(fence, line); !literal {
			lines[i] = codeSpans(line)
		}
	}
	return strings.Join(lines, "")
}

// textFence tracks text diff blocks line by line: given the fence
// open before line, it returns the fence open after it and whether line is
// part of a block, whose content is literal. Only the block's own fence
// closes it.
func textFence(fence, line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	switch {
	case fence != "":
		if strings.Trim(trimmed, "~") == "" && len(trimmed) >= len(fence) {
			return "", true
		}
		return fence, true
	case strings.HasPrefix(trimmed, "~~~") && strings.TrimLeft(trimmed, "~") == "diff":
		return strings.TrimSuffix(trimmed, "diff"), true
	}
	return "", false
}

func codeSpans(markdown string) string {
	return codeSpan.ReplaceAllStringFunc(markdown, func(span string) string {
		content := span[1 : len(span)-1]
		if content == "" {
			return "(none)"
		}
		if !strings.Contains(content, "&") {
			return span
		}
		return "<code>" + content + "</code>"
	})
}

// writeFence prints lines in a fenced diff block, each line after indent.
// Fenced content is shown literally, so it is not entity-escaped; control
// and format characters are escaped, each line is bounded to width runes and
// the fence is longer than any tilde run in the content, so a line cannot
// close it.
func writeFence(b *strings.Builder, indent string, lines []string, width int) {
	escaped := make([]string, len(lines))
	run := 0
	for i, line := range lines {
		escaped[i] = fenceText(line, width)
		run = max(run, longestRun(escaped[i], '~'))
	}
	fence := strings.Repeat("~", max(3, run+1))
	fmt.Fprintf(b, "%s%sdiff\n", indent, fence)
	for _, line := range escaped {
		fmt.Fprintf(b, "%s%s\n", indent, line)
	}
	fmt.Fprintf(b, "%s%s\n", indent, fence)
}

// fenceText escapes control and format characters and bounds a line to width
// runes.
func fenceText(s string, width int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == width {
			b.WriteString("…")
			break
		}
		if r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ') {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

func longestRun(s string, c rune) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == c {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}
