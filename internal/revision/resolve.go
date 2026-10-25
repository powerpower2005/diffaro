package revision

import (
	"context"
	"diffaro/internal/domain"
)

func Resolve(ctx context.Context, git domain.GitProvider, repository, base, head string, pr int) (domain.RevisionPair, error) {
	if pr > 0 {
		if base != "" || head != "" {
			return domain.RevisionPair{}, domain.Fail(domain.CodeConfigurationInvalid, "revision", "--pr cannot be combined with --base or --head", "choose exactly one comparison input mode", nil)
		}
		return git.PullRequest(ctx, repository, pr)
	}
	if pr < 0 || base == "" || head == "" {
		return domain.RevisionPair{}, domain.Fail(domain.CodeConfigurationInvalid, "revision", "both --base and --head are required without --pr", "provide two Git references or a PR number", nil)
	}
	b, err := git.Resolve(ctx, repository, base)
	if err != nil {
		return domain.RevisionPair{}, err
	}
	h := b
	if head != base {
		h, err = git.Resolve(ctx, repository, head)
		if err != nil {
			return domain.RevisionPair{}, err
		}
	}
	return domain.RevisionPair{Repository: repository, Base: b, Head: h, Mode: domain.RevisionModeExplicit}, nil
}
