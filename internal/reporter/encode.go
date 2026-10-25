package reporter

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

const MaxReportBytes = 256 << 20

// Encode redacts the report and writes it as json, markdown, pr-markdown or
// terminal. The human-readable formats are built from the redacted JSON, so
// they cannot show what it masks.
func Encode(report domain.Report, format string, redactor security.Redactor) ([]byte, error) {
	redacted, err := Redact(report, redactor)
	if err != nil {
		return nil, err
	}
	return redacted.Encode(format)
}

// Redacted is a report after the final redaction pass. It encodes several
// formats from the same redacted JSON, so a report written in two formats is
// redacted once and both outputs show the same masked facts.
type Redacted struct {
	// jsonLine is the redacted indented JSON with its trailing newline,
	// clipped so that appending to it never writes into shared memory.
	jsonLine []byte
}

// Redact runs the final redaction pass and checks the report byte budget.
func Redact(report domain.Report, redactor security.Redactor) (*Redacted, error) {
	raw, err := redactedJSON(report, redactor)
	if err != nil {
		return nil, err
	}
	return &Redacted{jsonLine: slices.Clip(append(raw, '\n'))}, nil
}

// Encode writes the redacted report as json, markdown, pr-markdown or
// terminal. The human-readable formats are built from a report decoded from
// the redacted JSON.
func (r *Redacted) Encode(format string) ([]byte, error) {
	if format == "json" {
		return r.jsonLine, nil
	}
	if !slices.Contains(Formats, format) {
		last := len(Formats) - 1
		return nil, errors.New("format must be " + strings.Join(Formats[:last], ", ") + " or " + Formats[last])
	}
	var safe domain.Report
	safeDecoder := json.NewDecoder(bytes.NewReader(r.jsonLine))
	safeDecoder.UseNumber()
	if err := safeDecoder.Decode(&safe); err != nil {
		return nil, err
	}
	v := newReportView(safe)
	switch format {
	case "pr-markdown":
		return encodePR(v), nil
	case "terminal":
		return encodeTerminal(v), nil
	}
	return encodeMarkdown(v)
}

// redactedJSON encodes the report as indented JSON after a final defensive
// redaction pass, which also cleans contextual identifiers and
// provider-derived strings, and checks the report byte budget.
func redactedJSON(report domain.Report, redactor security.Redactor) ([]byte, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	var object any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&object); err != nil {
		return nil, err
	}
	redacted := redactor.Value(object, "")
	redactChangeValues(redacted, object, redactor)
	raw, err = json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxReportBytes {
		return nil, domain.LimitError(domain.CodeReportLimitExceeded, "report", "encoded JSON report", "final report byte budget", int64(len(raw)), MaxReportBytes, "bytes", false, 0)
	}
	return raw, nil
}

// redactChangeValues redacts the before and after values of every change at
// the change's own JSON pointer, as the diff did, instead of at their
// position in the report. A Secret reference such as an ExternalSecret
// data[].secretKey is then shown and a literal value stays masked.
// redacted and original are the same report before and after the generic
// pass.
func redactChangeValues(redacted, original any, r security.Redactor) {
	field := func(v any, key string) any {
		m, _ := v.(map[string]any)
		return m[key]
	}
	each := func(redacted, original any, fn func(redacted, original any)) {
		out, _ := redacted.([]any)
		in, _ := original.([]any)
		for i := range min(len(out), len(in)) {
			fn(out[i], in[i])
		}
	}
	// prefix restores the pointer a change path is relative to.
	changes := func(redacted, original any, r security.Redactor, prefix string) {
		each(redacted, original, func(out, in any) {
			change, ok := out.(map[string]any)
			path, pathOK := field(in, "path").(string)
			if !ok || !pathOK {
				return
			}
			for _, side := range []string{"before", "after"} {
				// An absent side stays null: an added value has no before.
				if value := in.(map[string]any)[side]; value != nil {
					change[side] = r.Value(value, prefix+path)
				}
			}
		})
	}
	each(field(redacted, "applications"), field(original, "applications"), func(out, in any) {
		each(field(out, "resources"), field(in, "resources"), func(out, in any) {
			changes(field(out, "changes"), field(in, "changes"), r, "")
		})
		changes(field(out, "specChanges"), field(in, "specChanges"), r, "")
		// Value file changes were redacted for value files.
		// Inline value change paths are relative to the helm.values
		// pointer in the Application spec, which keeps them masked whole.
		each(field(out, "inputChanges"), field(in, "inputChanges"), func(out, in any) {
			inputs, prefix := r, ""
			switch field(in, "kind") {
			case "valueFile":
				inputs = r.ForValueFiles()
			case "inlineValues":
				prefix, _ = field(in, "path").(string)
			}
			changes(field(out, "changes"), field(in, "changes"), inputs, prefix)
		})
	})
}
