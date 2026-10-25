package preview

import (
	"context"
	"sync"
)

// flight shares one run's results per key. The first caller of a key
// computes it without holding the lock; concurrent callers wait for that
// call, and later callers reuse a kept result.
type flight[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

type flightCall[V any] struct {
	done chan struct{}
	val  V
	kept bool
}

// do returns key's kept result, waits for its call in flight, or calls fn.
// keep runs under the flight's lock once fn returns: a result it rejects,
// such as one of a cancelled or expired caller, goes to that caller only,
// and a waiter then calls do again. A waiter whose ctx ends first returns
// ctx.Err(). shared reports a result computed by another caller.
func (f *flight[K, V]) do(ctx context.Context, key K, fn func() V, keep func(V) bool) (val V, shared bool, err error) {
	for {
		f.mu.Lock()
		if f.calls == nil {
			f.calls = map[K]*flightCall[V]{}
		}
		call := f.calls[key]
		if call == nil {
			call = &flightCall[V]{done: make(chan struct{})}
			f.calls[key] = call
			f.mu.Unlock()
			return f.run(key, call, fn, keep), false, nil
		}
		f.mu.Unlock()
		// A finished call is reused even when ctx has ended since.
		select {
		case <-call.done:
		default:
			select {
			case <-call.done:
			case <-ctx.Done():
				var zero V
				return zero, false, ctx.Err()
			}
		}
		if call.kept {
			return call.val, true, nil
		}
	}
}

func (f *flight[K, V]) run(key K, call *flightCall[V], fn func() V, keep func(V) bool) V {
	defer close(call.done)
	finished := false
	defer func() {
		if !finished {
			// fn panicked: forget the call so its waiters run fn themselves
			// instead of finding the unkept call again and again.
			f.mu.Lock()
			delete(f.calls, key)
			f.mu.Unlock()
		}
	}()
	call.val = fn()
	finished = true
	f.mu.Lock()
	defer f.mu.Unlock()
	if call.kept = keep(call.val); !call.kept {
		delete(f.calls, key)
	}
	return call.val
}

// kept returns key's kept result without waiting for a call in flight.
func (f *flight[K, V]) kept(key K) (V, bool) {
	f.mu.Lock()
	call := f.calls[key]
	f.mu.Unlock()
	if call == nil {
		var zero V
		return zero, false
	}
	select {
	case <-call.done:
		return call.val, call.kept
	default:
		var zero V
		return zero, false
	}
}

// started reports whether key has a call in flight or a kept result.
func (f *flight[K, V]) started(key K) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key] != nil
}
