package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
)

// An explicit comparison that a render ends names the failing side and its
// revision, as a graph node does; a lookup failure belongs to neither side.
func TestExplicitFailureNamesTheFailingSide(t *testing.T) {
	c := windowConfig(8, 1)
	pair := domain.RevisionPair{Base: "base", Head: "head"}
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": &windowBackend{}}}.Run(context.Background(), pair)
	want := map[string]struct{ side, revision string }{"app-01": {"base", "base"}, "app-07": {"head", "head"}, "app-05": {"", ""}}
	for _, app := range report.Applications {
		expected, ok := want[app.ID.Name]
		if !ok {
			continue
		}
		delete(want, app.ID.Name)
		if len(app.Issues) != 1 || app.Issues[0].Side != expected.side || app.Issues[0].Revision != expected.revision {
			t.Fatalf("%s issues %+v, want side %q revision %q", app.ID.Name, app.Issues, expected.side, expected.revision)
		}
	}
	if len(want) != 0 {
		t.Fatalf("Applications not reported: %v", want)
	}
}
