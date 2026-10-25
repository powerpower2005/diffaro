package fetchcache

import (
	"context"
	"errors"
	"sync"
	"testing"

	"diffaro/internal/domain"
)

func TestTransientFetchFailuresAreRetriedAndDeterministicOnesKept(t *testing.T) {
	skipped := func(reason string) error {
		return &domain.RenderSkipped{Reason: reason, Cause: domain.Fail(domain.CodeLocalContextUnavailable, "render", reason, "check source", nil)}
	}
	var mu sync.Mutex
	m := map[string]*Entry[string]{}
	for _, tc := range []struct {
		name   string
		first  error
		cached bool
	}{
		{"server error", domain.Fail("backend_error", "connection", "status 502", "retry", nil), false},
		{"rate limit", domain.Fail("rate_limited", "connection", "quota", "wait", nil), false},
		{"timeout", domain.Fail("connection_failed", "connection", "timed out", "retry", nil), false},
		{"plain error", errors.New("connection reset"), false},
		{"marked transient", &TransientError{Err: skipped("public Helm dependency download is unavailable")}, false},
		{"not found", domain.Fail("not_found", "lookup", "missing", "check", nil), true},
		{"limit", domain.Fail("limit_exceeded", "source", "too large", "reduce", nil), true},
		{"skip", skipped("dependency URL requires unsupported credentials or transport"), true},
	} {
		fetches := 0
		fetch := func(context.Context) (string, error) {
			fetches++
			if fetches == 1 {
				return "", tc.first
			}
			return "ok", nil
		}
		_, err := Load(context.Background(), &mu, m, tc.name, fetch, nil)
		var marked *TransientError
		if err == nil || errors.As(err, &marked) {
			t.Fatalf("%s: first fetch error was not returned as its cause: %v", tc.name, err)
		}
		got, err := Load(context.Background(), &mu, m, tc.name, fetch, nil)
		if tc.cached && (err == nil || fetches != 1) {
			t.Fatalf("%s: deterministic failure was not kept for the run: %q %v fetches=%d", tc.name, got, err, fetches)
		}
		if !tc.cached && (err != nil || got != "ok" || fetches != 2) {
			t.Fatalf("%s: transient failure was replayed instead of retried: %v fetches=%d", tc.name, err, fetches)
		}
	}
}
