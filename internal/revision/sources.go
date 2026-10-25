package revision

import (
	"context"
	"regexp"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// SourceRevision retains source position: Argo CD's multi-source query uses
// one-based positions and source order also determines duplicate precedence.
type SourceRevision = domain.SourceRevision

type SourceResolver func(context.Context, string, string) (string, error)

var sourceRef = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// PlanSources does not rewrite the stored Application. A caller-provided
// resolver must freeze non-PR references across both comparison sides.
// aliases are explicitly configured URLs of the selected repository.
func PlanSources(ctx context.Context, app domain.Application, repository, revision string, resolve SourceResolver, aliases ...string) ([]SourceRevision, error) {
	if !domain.IsCommitSHA(revision) {
		return nil, domain.Fail(domain.CodeRevisionInvalid, "revision", "selected source revision is not an immutable commit", "resolve PR base/head before planning sources", nil)
	}
	var sources []any
	if raw, present := app.Spec["sources"]; present && raw != nil {
		var ok bool
		sources, ok = raw.([]any)
		if !ok {
			return nil, sourcePlanInvalid()
		}
	}
	if len(sources) == 0 && app.Source() != nil {
		sources = []any{app.Source()}
	}
	if len(sources) == 0 || len(sources) > 32 {
		return nil, sourcePlanInvalid()
	}
	refs := map[string]bool{}
	result := make([]SourceRevision, 0, len(sources))
	for i, raw := range sources {
		source, ok := raw.(map[string]any)
		if !ok || domain.String(source, "repoURL") == "" {
			return nil, sourcePlanInvalid()
		}
		ref := domain.String(source, "ref")
		if ref != "" && (!sourceRef.MatchString(ref) || refs[ref]) {
			return nil, sourcePlanInvalid()
		}
		if ref != "" {
			refs[ref] = true
		}
		item := SourceRevision{Position: i + 1, Repository: domain.String(source, "repoURL"), Ref: ref, Source: domain.CloneSpec(source), Revision: domain.String(source, "targetRevision"), Rule: "source-commit"}
		if profile.SameRepositoryAny(item.Repository, repository, aliases) {
			item.Revision, item.Rule = revision, "selected-repository-pair"
		} else if !domain.IsCommitSHA(item.Revision) {
			if domain.String(source, "tagPrefix") != "" {
				return nil, domain.SourceFailure(domain.Fail(domain.CodeRevisionMappingRequired, "revision", "tagPrefix requires an explicitly pinned non-PR source commit", "pin this source targetRevision to a full commit SHA", nil), item)
			}
			if resolve == nil {
				return nil, domain.SourceFailure(domain.Fail(domain.CodeRevisionMappingRequired, "revision", "a non-PR source requires a frozen revision", "pin its targetRevision to a full commit SHA, or select local Helm (--local-helm or --helm-binary) or the repo-server backend, which resolve a branch or tag once per run", nil), item)
			}
			var err error
			requested := item
			item.Revision, err = resolve(ctx, item.Repository, item.Revision)
			if err != nil {
				return nil, domain.SourceFailure(err, requested)
			}
			if !domain.IsCommitSHA(item.Revision) {
				return nil, domain.SourceFailure(domain.Fail(domain.CodeRevisionInvalid, "revision", "source resolver did not return an immutable commit", "check source revision resolution", nil), requested)
			}
			item.Rule, item.Requested = "resolved-source-reference", requested.Revision
		}
		item.Source["targetRevision"] = item.Revision
		result = append(result, item)
	}
	return result, nil
}

func sourcePlanInvalid() error {
	return domain.Fail(domain.CodeSourceInvalid, "revision", "sources must contain 1–32 source objects with repository URLs and unique valid refs", "correct the Application source definitions", nil)
}
