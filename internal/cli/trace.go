package cli

import (
	"encoding/json"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// TraceSchema versions the diagnostic trace written by diff --trace. It is independent of the report schema.
const TraceSchema = "diffaro.trace/v1alpha1"

type traceDocument struct {
	Schema    string              `json:"schema"`
	Generator *domain.Generator   `json:"generator,omitempty"`
	Revisions domain.RevisionPair `json:"revisions"`
	Status    string              `json:"status"`
	Dropped   int                 `json:"dropped"`
	// Failures holds the first failed, partial, timed-out or cancelled step
	// of every scope, in the order those steps ended. HTTP steps are left
	// out: a step can handle a failed request, such as a fallback after 404,
	// and a request failure it does not handle fails that step too.
	Failures []domain.TraceStep `json:"failures"`
	Steps    []domain.TraceStep `json:"steps"`
}

// encodeTrace redacts subjects and reasons like the report and lists the
// first failing step of each Application, side, revision and walk.
func encodeTrace(report domain.Report, trace *domain.Trace, redactor security.Redactor) ([]byte, error) {
	steps, dropped := trace.Steps()
	doc := traceDocument{Schema: TraceSchema, Generator: report.Generator, Revisions: report.Revisions, Status: string(report.Status), Dropped: dropped, Failures: []domain.TraceStep{}, Steps: steps}
	type scope struct {
		application, side, revision, walk string
	}
	failed := map[scope]bool{}
	for i := range doc.Steps {
		step := &doc.Steps[i]
		step.Subject, step.Reason = redactor.Text(step.Subject), redactor.Text(step.Reason)
		if step.Outcome == "ok" || step.Outcome == "skipped" || step.Phase == "http" {
			continue
		}
		key := scope{side: step.Side, revision: step.Revision, walk: step.Walk}
		if step.Application != nil {
			key.application = step.Application.String()
		}
		if !failed[key] {
			failed[key] = true
			doc.Failures = append(doc.Failures, *step)
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
