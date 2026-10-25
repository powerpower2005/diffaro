package localhelm

import (
	"context"
	"sync"
)

// renderMemo shares the render of identical inputs within one Client or
// External, such as the base and head of an unaffected Application. The
// caller that claims a key renders and settles it; callers of the same key
// wait for it and reuse it when it is kept. A render that is not kept is not
// shared: its waiters render themselves. Kept renders are bounded in bytes.
type renderMemo[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]*memoEntry[V]
	bytes   int64
}

// memoEntry is one render of identical inputs, running or settled. done
// closes when it is settled; kept reports whether value holds the render.
type memoEntry[V any] struct {
	done  chan struct{}
	kept  bool
	value V
}

// claim returns the entry of key and whether the caller owns it: the owner
// renders and then settles the entry; anyone else waits for it.
func (m *renderMemo[K, V]) claim(key K) (*memoEntry[V], bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.entries[key]; entry != nil {
		return entry, false
	}
	if m.entries == nil {
		m.entries = map[K]*memoEntry[V]{}
	}
	entry := &memoEntry[V]{done: make(chan struct{})}
	m.entries[key] = entry
	return entry, true
}

// settle ends the owner's render of key. A render that may be kept (ok) is
// kept, with value(), while the kept renders stay within limit bytes;
// otherwise the entry is removed, so that waiters and later callers render
// again.
func (m *renderMemo[K, V]) settle(key K, entry *memoEntry[V], ok bool, size, limit int64, value func() V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok && m.bytes+size <= limit {
		entry.kept, entry.value = true, value()
		m.bytes += size
	} else {
		delete(m.entries, key)
	}
	close(entry.done)
}

// wait waits for an entry another caller owns. It returns the kept value, or
// false when the render was not kept, or ctx.Err() when ctx ends first.
func (e *memoEntry[V]) wait(ctx context.Context) (V, bool, error) {
	select {
	case <-ctx.Done():
		var zero V
		return zero, false, ctx.Err()
	case <-e.done:
	}
	return e.value, e.kept, nil
}
