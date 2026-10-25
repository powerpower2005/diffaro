package revision

import (
	"context"
	"diffaro/internal/domain"
	"testing"
)

type fakeGit struct{ calls int }

func (f *fakeGit) Resolve(context.Context, string, string) (string, error) {
	f.calls++
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
}
func (f *fakeGit) PullRequest(context.Context, string, int) (domain.RevisionPair, error) {
	f.calls++
	return domain.RevisionPair{Mode: "pr-merge-base-head"}, nil
}

func TestInputModes(t *testing.T) {
	for _, tt := range []struct {
		base, head string
		pr         int
		valid      bool
	}{{"main", "feature", 0, true}, {"main", "main", 0, true}, {"", "", 1, true}, {"main", "feature", 1, false}, {"main", "", 0, false}, {"", "", -1, false}} {
		git := &fakeGit{}
		pair, err := Resolve(context.Background(), git, "example/charts", tt.base, tt.head, tt.pr)
		if (err == nil) != tt.valid {
			t.Fatalf("unexpected input validation: %v", err)
		}
		if !tt.valid && git.calls != 0 {
			t.Fatal("invalid input caused network access")
		}
		if tt.valid && tt.base == tt.head && tt.pr == 0 && (git.calls != 1 || pair.Base != pair.Head) {
			t.Fatal("same ref must be pinned once")
		}
	}
}
