package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// TraceStep is one diagnostic record of an opt-in phase trace. It
// is never part of Report: timing and order depend on scheduling.
type TraceStep struct {
	Seq         int            `json:"seq"`
	Application *ApplicationID `json:"application,omitempty"`
	Side        string         `json:"side,omitempty"`
	Revision    string         `json:"revision,omitempty"`
	Walk        string         `json:"walk,omitempty"`
	Phase       string         `json:"phase"`
	Step        string         `json:"step"`
	// Subject identifies what the step read: a repository, revision,
	// repository-relative path, route template or resource identity. Never
	// a URL with query, a credential, a value or file content.
	Subject string `json:"subject,omitempty"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Status is the HTTP status code of an http step, 0 without a response.
	Status int `json:"status,omitempty"`
	// Digest identifies what a successful step read or produced
	//: "sha256:" over a whole object, or "git:" with a commit.
	// It never covers a single value on its own.
	Digest string `json:"digest,omitempty"`
	// Diagnostic classifies a failed step like an issue's diagnostic
	//, also when the error carries no issue.
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
	StartMS    int64       `json:"startMs"`
	DurationMS int64       `json:"durationMs"`
}

// MaxTraceSteps bounds a trace; later steps are counted as dropped.
const MaxTraceSteps = 200_000

// Trace collects steps from concurrent callers of one run.
type Trace struct {
	mu      sync.Mutex
	start   time.Time
	now     func() time.Time
	steps   []TraceStep
	dropped int
}

func NewTrace() *Trace { return &Trace{start: time.Now(), now: time.Now} }

// Steps returns the recorded steps in completion order and the number of
// steps beyond MaxTraceSteps.
func (t *Trace) Steps() ([]TraceStep, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceStep{}, t.steps...), t.dropped
}

func (t *Trace) record(step TraceStep) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.steps) >= MaxTraceSteps {
		t.dropped++
		return
	}
	step.Seq = len(t.steps) + 1
	t.steps = append(t.steps, step)
}

type traceKey struct{}

type traceScope struct {
	trace       *Trace
	application *ApplicationID
	side        string
	revision    string
	walk        string
}

func scopeOf(ctx context.Context) (traceScope, bool) {
	s, ok := ctx.Value(traceKey{}).(traceScope)
	return s, ok && s.trace != nil
}

// WithTrace attaches t to ctx. Without it every trace call is a no-op.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, traceScope{trace: t})
}

// TraceApplication scopes later steps to one Application.
func TraceApplication(ctx context.Context, id ApplicationID) context.Context {
	s, ok := scopeOf(ctx)
	if !ok {
		return ctx
	}
	s.application = &id
	return context.WithValue(ctx, traceKey{}, s)
}

// TraceRevision scopes later steps to one side ("base" or "head") and its
// revision.
func TraceRevision(ctx context.Context, side, revision string) context.Context {
	s, ok := scopeOf(ctx)
	if !ok {
		return ctx
	}
	s.side, s.revision = side, revision
	return context.WithValue(ctx, traceKey{}, s)
}

// TraceWalk names the graph walk that later steps belong to.
func TraceWalk(ctx context.Context, walk string) context.Context {
	s, ok := scopeOf(ctx)
	if !ok {
		return ctx
	}
	s.walk = walk
	return context.WithValue(ctx, traceKey{}, s)
}

// TraceStart begins a step and returns the function that ends it with the
// step's error. It costs nothing when ctx carries no trace.
func TraceStart(ctx context.Context, phase, step, subject string) func(error) {
	end := traceBegin(ctx, phase, step, subject)
	return func(err error) { end(0, nil, err) }
}

// TraceRead begins a step whose result has an identity, such as a spec read
// or a render output. digest runs only with a trace, and only when the step
// succeeded or rendered partially, so a failure never reports an identity.
func TraceRead(ctx context.Context, phase, step, subject string) func(digest func() string, err error) {
	end := traceBegin(ctx, phase, step, subject)
	return func(digest func() string, err error) { end(0, digest, err) }
}

// DigestJSON is the "sha256:" digest of v's JSON encoding, whose object keys
// are sorted, so equal objects have equal digests across runs.
func DigestJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TraceHTTP begins one provider request step. route must be the fixed route
// template, never the request URL; end it with the response status (0
// without a response) and the request error.
func TraceHTTP(ctx context.Context, service, operation, route string) func(int, error) {
	end := traceBegin(ctx, "http", service+" "+operation, route)
	return func(status int, err error) { end(status, nil, err) }
}

func traceBegin(ctx context.Context, phase, step, subject string) func(int, func() string, error) {
	s, ok := scopeOf(ctx)
	if !ok {
		return func(int, func() string, error) {}
	}
	started := s.trace.now()
	return func(status int, digest func() string, err error) {
		ended := s.trace.now()
		record := TraceStep{Application: s.application, Side: s.side, Revision: s.revision, Walk: s.walk, Phase: phase, Step: step, Subject: subject, Status: status, StartMS: started.Sub(s.trace.start).Milliseconds(), DurationMS: ended.Sub(started).Milliseconds()}
		record.Outcome, record.Code, record.Stage, record.Reason = traceOutcome(ctx, err)
		record.Diagnostic = traceDiagnostic(err)
		if digest != nil && (err == nil || IsRenderPartial(err)) {
			record.Digest = digest()
		}
		s.trace.record(record)
	}
}

// traceOutcome classifies a step error without copying its free-form text:
// only the structured issue code, stage and reason are kept.
func traceOutcome(ctx context.Context, err error) (outcome, code, stage, reason string) {
	if err == nil {
		return "ok", "", "", ""
	}
	var skipped *RenderSkipped
	var failure *Failure
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "timeout"
	case errors.Is(err, context.Canceled):
		outcome = "cancelled"
	case errors.As(err, &skipped):
		return "skipped", "", "", skipped.Reason
	case IsRenderPartial(err):
		outcome = "partial"
	default:
		outcome = "failed"
		if ctx.Err() != nil {
			// A provider error caused by this step's own deadline or
			// cancellation is reported as such, not as a backend failure.
			outcome = "timeout"
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome = "cancelled"
			}
		}
	}
	if errors.As(err, &failure) {
		code, stage, reason = failure.Issue.Code, failure.Issue.Stage, failure.Issue.Reason
	}
	return outcome, code, stage, reason
}

// traceDiagnostic is the diagnostic of a failed step: its failure's, or the
// cause chain of an error without one.
func traceDiagnostic(err error) *Diagnostic {
	var failure *Failure
	var skipped *RenderSkipped
	switch {
	case err == nil || errors.As(err, &skipped):
		return nil
	case errors.As(err, &failure):
		return failure.Issue.Diagnostic
	}
	return UntypedDiagnostic(err)
}
