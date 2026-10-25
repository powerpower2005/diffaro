// Package fetchcache shares fetches within a run while preserving cancellation
// and retry semantics. Callers own the maps and retention budgets.
package fetchcache

import (
	"context"
	"errors"
	"sync"

	"diffaro/internal/domain"
)

// Entry is one cached fetch. Callers of the same key wait for the first
// fetch instead of repeating it, without holding a lock during network I/O.
type Entry[T any] struct {
	done  chan struct{}
	value T
	err   error
	// abandoned is set when the fetching caller's context ended; waiters
	// then retry with their own context instead of inheriting its error.
	abandoned bool
}

// Load returns the value cached under key in m, which mu guards. The first
// caller fetches with its own context; later callers wait for that fetch or
// their own cancellation. A fetch failure caused by the fetcher's cancellation
// is not cached and does not fail other waiters. Deterministic failures (see
// settled) are cached like successes, so a run sees one outcome per immutable
// key. Any other failure, such as a timeout, rate limit or server error, is
// returned to the current waiters and then dropped so a later caller retries
// instead of failing every later Application. keep, when set,
// runs under mu after a successful fetch; false delivers the value to current
// waiters without retaining it.
func Load[T any](ctx context.Context, mu *sync.Mutex, m map[string]*Entry[T], key string, fetch func(context.Context) (T, error), keep func(T) bool) (T, error) {
	for {
		mu.Lock()
		f := m[key]
		if f == nil {
			f = &Entry[T]{done: make(chan struct{})}
			m[key] = f
			mu.Unlock()
			return f.run(ctx, mu, m, key, fetch, keep)
		}
		mu.Unlock()
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-f.done:
		}
		if !f.abandoned {
			return f.value, f.err
		}
	}
}

func (f *Entry[T]) run(ctx context.Context, mu *sync.Mutex, m map[string]*Entry[T], key string, fetch func(context.Context) (T, error), keep func(T) bool) (T, error) {
	defer close(f.done)
	// A panicking fetch fails like any unsettled fetch: its waiters get the
	// failure, not an empty value, and a later caller retries.
	f.value, f.err = func() (value T, err error) {
		defer domain.RecoverAs(&err, "source")
		return fetch(ctx)
	}()
	retry := false
	if t, ok := f.err.(*TransientError); ok {
		f.err, retry = t.Err, true
	}
	mu.Lock()
	defer mu.Unlock()
	switch {
	case f.err != nil && ctx.Err() != nil:
		f.abandoned = true
		delete(m, key)
	case f.err != nil && (retry || !settled(f.err)):
		delete(m, key)
	case f.err == nil && keep != nil && !keep(f.value):
		delete(m, key)
	}
	return f.value, f.err
}

// settled reports whether a fetch failure is a deterministic outcome of its
// key that repeating the fetch in the same run would only reproduce. Unknown
// and transport failures are not settled.
func settled(err error) bool {
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Issue.Code {
	case domain.CodeLimitExceeded, domain.CodeNotFound, domain.CodeAccessDenied, domain.CodeRevisionInvalid, domain.CodeRequestInvalid, domain.CodeUnsupportedSource, domain.CodeDestinationInvalid, domain.CodeLocalContextUnavailable, domain.CodeLocalRenderFailed:
		return true
	}
	return false
}

// TransientError marks a fetch failure that does not describe its key, such
// as an interrupted download, when its code would otherwise be settled. Load
// returns the wrapped error and does not cache it.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Cached returns a successful settled value without waiting for a fetch.
// Values are immutable after the fetch completes.
func (f *Entry[T]) Cached() (T, bool) {
	if f != nil {
		select {
		case <-f.done:
			return f.value, f.err == nil && !f.abandoned
		default:
		}
	}
	var zero T
	return zero, false
}
