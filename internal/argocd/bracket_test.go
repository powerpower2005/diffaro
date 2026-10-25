package argocd

import (
	"context"
	"errors"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// Bracket reads the stored Application before and after every render whose
// targets are usable, a partial render included, and returns the caller's
// drift error for a change. A failed render returns at once: its targets are
// not used, so no read after it can matter.
func TestBracketChecksDriftAroundUsableRenders(t *testing.T) {
	spec := map[string]any{"project": "demo", "source": map[string]any{"repoURL": "https://example.com/team/chart", "path": "chart", "helm": map[string]any{}}}
	app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "child"}, Spec: spec}
	before, during := errors.New("changed before"), errors.New("changed during")
	partial := &domain.RenderPartial{Cause: domain.Fail(domain.CodeLocalRenderFailed, "render", "one template failed", "", nil)}
	failed := domain.Fail(domain.CodeLocalRenderFailed, "render", "render failed", "", nil)
	for _, tc := range []struct {
		name      string
		renderErr error
		drift     bool
		reads     int32
		want      error
	}{
		{"complete", nil, false, 2, nil},
		{"partial", partial, false, 2, partial},
		{"partial with drift", partial, true, 2, during},
		{"complete with drift", nil, true, 2, during},
		{"failed", failed, true, 1, failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, apps, _, renders := renderCallServer(t, spec, tc.drift)
			defer server.Close()
			c := &Client{Instance: "test", HTTP: httpx.New(server.URL, "", time.Second, 1<<20)}
			out, err := c.Bracket(context.Background(), app, before, during, func() ([]string, error) {
				renders.Add(1)
				return []string{"target"}, tc.renderErr
			})
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) || apps.Load() != tc.reads {
				t.Fatalf("err=%v reads=%d", err, apps.Load())
			}
			if usable := tc.want == nil || tc.want == partial; usable != (len(out) == 1) {
				t.Fatalf("targets=%v with err=%v", out, err)
			}
		})
	}
}
