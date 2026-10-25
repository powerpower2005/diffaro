package preview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
)

// expansionCache reuses one run's render result for identical inputs, so a
// candidate probe and the final graph walk render each input once.
// Concurrent walks wait for an in-flight render of the same input instead of
// repeating it. Context cancellation and deadlines are transient and never
// retained; a waiter then renders the input again.
type expansionCache struct {
	renders flight[expansionKey, expansionResult]
	// used counts the bytes of kept results; the renders flight's lock,
	// under which keep runs, guards it.
	used, limit int
	// slots bound the run's background child expansions (render.maxConcurrency).
	slots chan struct{}
	// independent, when set, reports inputs whose expansion does not depend
	// on the walked revision; base and head then share one render.
	independent func(domain.Application, bool) bool
}
type expansionKey struct {
	revision string
	root     bool
	id       domain.ApplicationID
	spec     [32]byte
}
type expansionResult struct {
	exp graph.Expansion
	err error
	// size is the encoded size of exp, or -1 for a transient or
	// unencodable result, which is never kept.
	size int
}

func newExpansionCache(limit, concurrency int) *expansionCache {
	return &expansionCache{limit: limit, slots: make(chan struct{}, max(concurrency, 1))}
}

func (c *expansionCache) key(revision string, app domain.Application, root bool) (expansionKey, bool) {
	if c.independent != nil && c.independent(app, root) {
		revision = ""
	}
	spec, err := json.Marshal(app.Spec)
	if err != nil {
		return expansionKey{}, false
	}
	return expansionKey{revision: revision, root: root, id: app.ID, spec: sha256.Sum256(spec)}, true
}

// prefetch expands children in the background as the wrapped expand of the
// same revision does, so the ordered walk finds them in flight or finished. A
// child that is already in flight or cached is left to it. wg is done once
// every prefetch of the call has returned.
func (c *expansionCache) prefetch(ctx context.Context, wg *sync.WaitGroup, revision string, expand expandFunc, children []domain.Application) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A prefetch only warms the cache: after a panic the ordered walk
		// expands the child itself and reports the failure.
		defer func() { _ = recover() }()
		for _, child := range children {
			select {
			case c.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			key, ok := c.key(revision, child, false)
			if !ok || c.renders.started(key) {
				<-c.slots
				continue
			}
			wg.Add(1)
			go func() {
				defer func() { <-c.slots; wg.Done() }()
				defer func() { _ = recover() }()
				_, _ = c.expand(ctx, revision, key, expand, child, false)
			}()
		}
	}()
}

type expandFunc = func(context.Context, domain.Application, bool) (graph.Expansion, error)

func (c *expansionCache) wrap(revision string, expand expandFunc) expandFunc {
	return func(ctx context.Context, app domain.Application, root bool) (graph.Expansion, error) {
		key, ok := c.key(revision, app, root)
		if !ok {
			return expand(ctx, app, root)
		}
		return c.expand(ctx, revision, key, expand, app, root)
	}
}

// expand returns key's shared expansion or expands app, measuring a kept
// result once for the cache budget and every walk (graph.Expansion.ResourceBytes).
func (c *expansionCache) expand(ctx context.Context, revision string, key expansionKey, expand expandFunc, app domain.Application, root bool) (graph.Expansion, error) {
	result, shared, err := c.renders.do(ctx, key, func() expansionResult {
		r := expansionResult{size: -1}
		r.exp, r.err = expand(ctx, app, root)
		if ctx.Err() == nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
			r.exp.ResourceBytes = graph.ResourceSize(r.exp.Resources)
			r.size = expansionSize(r.exp)
		}
		return r
	}, func(r expansionResult) bool {
		// Unstored results only cost a repeated render; the budget bounds memory.
		if r.size < 0 || r.size > c.limit-c.used {
			return false
		}
		c.used += r.size
		return true
	})
	if err != nil {
		// The leader's render is bounded by its own stage timeout.
		return graph.Expansion{}, err
	}
	if shared {
		// A reused result, including a failure, came from another
		// caller's render in this run.
		domain.TraceStart(domain.TraceApplication(ctx, app.ID), "render", "expansion-cache", revision)(result.err)
	}
	return result.exp, result.err
}

// expansionSize returns the JSON size of exp, as json.Marshal(exp) would,
// from its measured ResourceBytes instead of encoding the resources again.
// It is -1 when exp cannot be encoded.
func expansionSize(exp graph.Expansion) int {
	if exp.ResourceBytes < 0 {
		return -1
	}
	rest := exp
	rest.Resources = nil
	encoded, err := json.Marshal(rest)
	if err != nil {
		return -1
	}
	// rest encodes its resources as null.
	return len(encoded) - len("null") + exp.ResourceBytes
}
