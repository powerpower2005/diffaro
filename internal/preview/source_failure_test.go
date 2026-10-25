package preview

import (
	"context"
	"errors"
	"testing"

	"diffaro/internal/domain"
)

func TestSourceFailureContextSurvivesPreviewAndDoesNotMutateCause(t *testing.T) {
	original := domain.Fail("access_denied", "permission", "repository not allowed", "review project sources", nil)
	source := domain.SourceRevision{Position: 2, Repository: "https://example.com/values", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	wrapped := domain.SourceFailure(original, source)
	if !errors.Is(wrapped, original) || original.(*domain.Failure).Issue.Repository != "" {
		t.Fatal("source context mutated the original failure")
	}
	issue := ContextualIssue(wrapped, domain.ApplicationID{Instance: "one", Name: "app"}, "https://example.com/pr", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if issue.Repository != source.Repository || issue.Revision != source.Revision || issue.SourcePosition != 2 || issue.Stage != "permission" {
		t.Fatal("preview replaced source context with PR context")
	}
	issue = ContextualIssue(domain.SourceFailure(context.DeadlineExceeded, source), domain.ApplicationID{}, "", "")
	if issue.Code != "timeout" || issue.SourcePosition != 2 {
		t.Fatal("source timeout classification lost")
	}
}
