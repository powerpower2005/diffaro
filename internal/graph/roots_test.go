package graph

import (
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

func TestSelectInventoryRoots(t *testing.T) {
	snapshot := func(names ...string) Snapshot {
		s := Snapshot{Nodes: map[domain.ApplicationID]*Node{}, Complete: true}
		for _, name := range names {
			a := app(name)
			s.Nodes[a.ID] = &Node{Application: a}
		}
		return s
	}
	candidate := func(name string, base, head []string) CandidateGraphs {
		return CandidateGraphs{Application: app(name), Base: snapshot(append(base, name)...), Head: snapshot(append(head, name)...)}
	}
	for _, tt := range []struct {
		name              string
		candidates        []CandidateGraphs
		roots, unresolved []string
	}{
		{"independent", []CandidateGraphs{candidate("b", nil, nil), candidate("a", nil, nil)}, []string{"a", "b"}, nil},
		{"removed child stays owned", []CandidateGraphs{candidate("parent", []string{"child"}, nil), candidate("child", nil, nil)}, []string{"parent"}, nil},
		{"added child stays owned", []CandidateGraphs{candidate("parent", nil, []string{"child"}), candidate("child", nil, nil)}, []string{"parent"}, nil},
		{"shared child", []CandidateGraphs{candidate("a", []string{"child"}, []string{"child"}), candidate("b", []string{"child"}, []string{"child"}), candidate("child", nil, nil)}, []string{"a", "b"}, nil},
		{"obsolete stored child edges do not hide independent app", []CandidateGraphs{candidate("parent", []string{"child"}, []string{"child"}), candidate("child", []string{"independent"}, []string{"independent"}), candidate("independent", nil, nil)}, []string{"independent", "parent"}, nil},
		{"deep desired graph", []CandidateGraphs{candidate("parent", []string{"child", "leaf"}, []string{"child", "leaf"}), candidate("child", []string{"leaf"}, []string{"leaf"}), candidate("leaf", nil, nil)}, []string{"parent"}, nil},
		{"cycle preserves independent root", []CandidateGraphs{candidate("a", []string{"b"}, []string{"b"}), candidate("b", []string{"a"}, []string{"a"}), candidate("other", nil, nil)}, []string{"other"}, []string{"a", "b"}},
		{"ownership reversal is explicit ambiguity", []CandidateGraphs{candidate("a", []string{"b"}, nil), candidate("b", nil, []string{"a"})}, nil, []string{"a", "b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for pass := 0; pass < 2; pass++ {
				roots, unresolved := SelectRoots(tt.candidates)
				var names, missing []string
				for _, root := range roots {
					names = append(names, root.ID.Name)
				}
				for _, id := range unresolved {
					missing = append(missing, id.Name)
				}
				if !reflect.DeepEqual(names, tt.roots) || !reflect.DeepEqual(missing, tt.unresolved) {
					t.Fatalf("roots=%v unresolved=%v", names, missing)
				}
				for i, j := 0, len(tt.candidates)-1; i < j; i, j = i+1, j-1 {
					tt.candidates[i], tt.candidates[j] = tt.candidates[j], tt.candidates[i]
				}
			}
		})
	}
}
