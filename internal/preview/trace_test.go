package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type secretBackend struct{ limitedBackend }

func (*secretBackend) Render(context.Context, domain.Application, string) ([]string, error) {
	return []string{`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"password":"++++++++"}}`}, nil
}

func tracedRun(t *testing.T, backend domain.Renderer, timeout string) []domain.TraceStep {
	t.Helper()
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: timeout, MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	trace := domain.NewTrace()
	Runner{Config: c, Renderers: map[string]domain.Renderer{"test": backend}}.Run(domain.WithTrace(context.Background(), trace), domain.RevisionPair{Base: "base", Head: "head"})
	steps, _ := trace.Steps()
	return steps
}

func findStep(steps []domain.TraceStep, phase, step string) *domain.TraceStep {
	for i := range steps {
		if steps[i].Phase == phase && steps[i].Step == step {
			return &steps[i]
		}
	}
	return nil
}

// A render that hits the stage deadline is recorded as a timeout of that
// render on its side, not as a generic failure of the Application.
func TestTraceLocatesRenderTimeout(t *testing.T) {
	steps := tracedRun(t, &limitedBackend{timeout: true}, "1ms")
	lookup, render, result := findStep(steps, "lookup", "application"), findStep(steps, "render", "stored"), findStep(steps, "result", "application")
	if lookup == nil || lookup.Outcome != "ok" || render == nil || result == nil {
		t.Fatalf("steps: %+v", steps)
	}
	if render.Outcome != "timeout" || render.Side != "base" || render.Revision != "base" || render.Walk != "explicit" || render.Application == nil || render.Application.Name != "api" {
		t.Fatalf("render timeout not located: %+v", render)
	}
	if result.Outcome != "timeout" || result.Seq < render.Seq {
		t.Fatalf("Application result does not follow its failing step: %+v", result)
	}
}

// A Secret whose payload Argo CD masked stays an unknown resource; no step
// of the run fails because of it.
func TestTraceMaskedSecretDoesNotFailTheRun(t *testing.T) {
	steps := tracedRun(t, &secretBackend{}, "1s")
	for _, step := range steps {
		if step.Outcome != "ok" {
			t.Fatalf("step %s/%s did not succeed: %+v", step.Phase, step.Step, step)
		}
	}
	if parse := findStep(steps, "parse", "target"); parse == nil {
		t.Fatal("parse step missing before the diff")
	}
}

// Both modes record the diff of every compared Application and then its
// result, also when the comparison succeeds.
func TestTraceRecordsDiffAndResultInBothModes(t *testing.T) {
	steps := tracedRun(t, &limitedBackend{}, "1s")
	diff, result := findStep(steps, "diff", "application"), findStep(steps, "result", "application")
	if diff == nil || result == nil || diff.Outcome != "ok" || result.Outcome != "ok" || result.Seq < diff.Seq || diff.Application == nil || diff.Application.Name != "api" {
		t.Fatalf("explicit mode must record the diff, then a successful result: %+v", steps)
	}
	trace := domain.NewTrace()
	graphRunner(sideFailures(nil, nil), "root").RunRecursive(domain.WithTrace(context.Background(), trace), graphPair())
	steps, _ = trace.Steps()
	if diff := findStep(steps, "diff", "application"); diff == nil || diff.Application == nil || diff.Application.Name != "root" {
		t.Fatalf("graph mode must record the diff of each Application: %+v", steps)
	}
}

// A head side whose revision equals the base revision is still labelled
// head, in explicit and graph mode. In graph mode the head walk reuses the
// base render of identical inputs (render/expansion-cache).
func TestTraceLabelsSidesWhenBaseEqualsHead(t *testing.T) {
	same := domain.RevisionPair{Repository: "example/charts", Base: graphBase, Head: graphBase}
	runner := graphRunner(sideFailures(nil, nil), "root")
	for name, run := range map[string]func(context.Context){
		"explicit":  func(ctx context.Context) { runner.Run(ctx, same) },
		"recursive": func(ctx context.Context) { runner.RunRecursive(ctx, same) },
	} {
		trace := domain.NewTrace()
		run(domain.WithTrace(context.Background(), trace))
		steps, _ := trace.Steps()
		sides := map[string]bool{}
		for _, step := range steps {
			if step.Phase == "parse" || step.Phase == "render" {
				sides[step.Side] = true
			}
		}
		if !sides["base"] || !sides["head"] {
			t.Fatalf("%s: render or parse steps must be recorded on both sides: %v", name, sides)
		}
	}
}

// Tracing is diagnostic only: the report is byte-identical without it.
func TestTraceDoesNotChangeTheReport(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Applications: []domain.ApplicationID{id}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 1, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	run := func(ctx context.Context) []byte {
		report := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": &secretBackend{}}}.Run(ctx, domain.RevisionPair{Base: "base", Head: "head"})
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	trace := domain.NewTrace()
	traced, plain := run(domain.WithTrace(context.Background(), trace)), run(context.Background())
	if steps, _ := trace.Steps(); len(steps) == 0 || !bytes.Equal(traced, plain) {
		t.Fatal("tracing changed the comparison or recorded nothing")
	}
}
