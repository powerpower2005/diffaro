package preview

import (
	"context"
	"encoding/json"
	"errors"

	"diffaro/internal/domain"
	"diffaro/internal/revision"
)

type frozenSourceRevision struct {
	sha string
	err error
}

// Owned by a single comparison, including graph probes and both graph walks.
// No credential, rendered value, or resolution survives into another run.
type sourceRevisionCache struct {
	resolved flight[string, frozenSourceRevision]
}

func newSourceRevisionCache() *sourceRevisionCache {
	return &sourceRevisionCache{}
}

// sourceResolver resolves a non-PR source reference through the Application's
// backend, once per run and authorization scope. It is nil when the backend
// cannot resolve references.
func (r *run) sourceResolver(app domain.Application) revision.SourceResolver {
	b := r.backends[app.ID.Instance]
	if b == nil || b.resolver == nil {
		return nil
	}
	backend := b.resolver
	return func(ctx context.Context, repository, reference string) (string, error) {
		// Exact URL and full authorization scope prevent credential or policy
		// decisions from being reused across instances, projects or Applications.
		keyBytes, err := json.Marshal([]any{app.ID, app.Project(), app.Spec["destination"], repository, reference})
		if err != nil {
			return "", domain.Fail(domain.CodeSourceInvalid, "revision", "invalid source authorization scope", "correct the Application destination", nil)
		}
		frozen, _, err := r.sourceRevisions.resolved.do(ctx, string(keyBytes), func() frozenSourceRevision {
			sha, err := backend.ResolveSourceRevision(ctx, app, repository, reference)
			return frozenSourceRevision{sha, err}
		}, func(f frozenSourceRevision) bool {
			// Cancellation and deadlines belong to this caller; later callers
			// resolve again instead of replaying them.
			return ctx.Err() == nil && !errors.Is(f.err, context.Canceled) && !errors.Is(f.err, context.DeadlineExceeded)
		})
		if err != nil {
			return "", err
		}
		return frozen.sha, frozen.err
	}
}

// tracedSourceResolver bounds and traces each resolution of a single-source
// node's non-PR reference. It is nil when no resolver is configured.
func (r *run) tracedSourceResolver(app domain.Application) revision.SourceResolver {
	resolve := r.sourceResolver(app)
	if resolve == nil {
		return nil
	}
	return func(ctx context.Context, repository, reference string) (string, error) {
		stage, cancel := context.WithTimeout(ctx, r.cfg.Timeout())
		defer cancel()
		end := domain.TraceRead(stage, "revision", "source-reference", repository+"@"+reference)
		sha, err := resolve(stage, repository, reference)
		end(func() string { return "git:" + sha }, err)
		return sha, err
	}
}

// withoutSourcePosition keeps a single-source node's issue shape: the source
// position PlanSources attaches is only meaningful for multi-source nodes.
func withoutSourcePosition(err error) error {
	if f, ok := err.(*domain.Failure); ok && f.Issue.SourcePosition > 0 && f.Cause != nil {
		return f.Cause
	}
	return err
}
