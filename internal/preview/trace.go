package preview

import (
	"context"

	"diffaro/internal/domain"
)

// traceSide scopes trace steps to one side ("base" or "head"), its revision
// and a walk. The side is named by the caller: base and head can be the same
// revision.
func traceSide(ctx context.Context, side, revision, walk string) context.Context {
	return domain.TraceWalk(domain.TraceRevision(ctx, side, revision), walk)
}
