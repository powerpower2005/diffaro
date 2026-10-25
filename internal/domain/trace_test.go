package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTraceIsNoOpWithoutTrace(t *testing.T) {
	ctx := TraceWalk(TraceRevision(TraceApplication(context.Background(), ApplicationID{Name: "a"}), "base", "b"), "final")
	if ctx.Value(traceKey{}) != nil {
		t.Fatal("scope helpers attached a scope without a trace")
	}
	TraceStart(ctx, "render", "helm", "")(errors.New("ignored"))
}

func TestTraceRecordsScopeOutcomeAndTiming(t *testing.T) {
	trace := NewTrace()
	clock := trace.start
	trace.now = func() time.Time { clock = clock.Add(5 * time.Millisecond); return clock }
	ctx := WithTrace(context.Background(), trace)
	app := ApplicationID{Instance: "dev", Namespace: "argocd", Name: "api"}
	scoped := TraceWalk(TraceRevision(TraceApplication(ctx, app), "head", "bbbb"), "final")
	deadline, cancel := context.WithDeadline(scoped, time.Unix(0, 0))
	defer cancel()
	for _, tc := range []struct {
		ctx  context.Context
		err  error
		want []string
	}{
		{scoped, nil, []string{"ok", "", "", ""}},
		{scoped, Fail("not_found", "source", "values file is missing", "", nil), []string{"failed", "not_found", "source", "values file is missing"}},
		{scoped, &RenderSkipped{Reason: "outside the local context"}, []string{"skipped", "", "", "outside the local context"}},
		{scoped, &RenderPartial{Cause: Fail("local_render_failed", "render", "template failed", "", nil)}, []string{"partial", "local_render_failed", "render", "template failed"}},
		{scoped, fmt.Errorf("wrapped: %w", context.Canceled), []string{"cancelled", "", "", ""}},
		// A provider failure caused by the step's own deadline is a timeout.
		{deadline, Fail("connection_failed", "connection", "API request failed or timed out", "", nil), []string{"timeout", "connection_failed", "connection", "API request failed or timed out"}},
		// Free-form error text is never copied: it can hold a URL.
		{scoped, errors.New("Get https://user:secret@example.invalid/x"), []string{"failed", "", "", ""}},
	} {
		TraceStart(tc.ctx, "render", "helm", "charts/api")(tc.err)
		steps, _ := trace.Steps()
		got := steps[len(steps)-1]
		if outcome := []string{got.Outcome, got.Code, got.Stage, got.Reason}; strings.Join(outcome, "|") != strings.Join(tc.want, "|") {
			t.Errorf("err=%v: got %q want %q", tc.err, outcome, tc.want)
		}
		if got.Application == nil || *got.Application != app || got.Side != "head" || got.Revision != "bbbb" || got.Walk != "final" || got.Subject != "charts/api" || got.DurationMS != 5 || got.Seq != len(steps) {
			t.Fatalf("scope or timing lost: %+v", got)
		}
	}
}

func TestTraceIsBoundedAndSafeForConcurrentSteps(t *testing.T) {
	trace := NewTrace()
	ctx := WithTrace(context.Background(), trace)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range MaxTraceSteps/8 + 10 {
				TraceStart(ctx, "http", "get", "")(nil)
			}
		}()
	}
	wg.Wait()
	steps, dropped := trace.Steps()
	if len(steps) != MaxTraceSteps || dropped != 80 {
		t.Fatalf("steps=%d dropped=%d", len(steps), dropped)
	}
	for i, step := range steps {
		if step.Seq != i+1 {
			t.Fatalf("sequence gap at %d: %d", i, step.Seq)
		}
	}
}

func TestTraceDigestsAreLazyStableAndOnlyForResults(t *testing.T) {
	calls := 0
	digest := func() string { calls++; return DigestJSON(map[string]any{"b": 2, "a": []any{"x"}}) }
	TraceRead(context.Background(), "lookup", "application", "")(digest, nil)
	if calls != 0 {
		t.Fatal("digest computed without a trace")
	}
	trace := NewTrace()
	ctx := WithTrace(context.Background(), trace)
	TraceRead(ctx, "lookup", "application", "")(digest, nil)
	TraceRead(ctx, "render", "helm", "")(digest, &RenderPartial{Cause: errors.New("template failed")})
	TraceRead(ctx, "source", "files", "")(digest, Fail("not_found", "source", "missing", "", nil))
	steps, _ := trace.Steps()
	want := DigestJSON(map[string]any{"a": []any{"x"}, "b": 2})
	if calls != 2 || steps[0].Digest != want || steps[1].Digest != want || steps[2].Digest != "" {
		t.Fatalf("calls=%d steps=%+v", calls, steps)
	}
	if !strings.HasPrefix(want, "sha256:") || len(want) != len("sha256:")+64 {
		t.Fatalf("digest format %q", want)
	}
}
