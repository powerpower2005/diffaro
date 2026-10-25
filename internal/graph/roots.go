package graph

import (
	"sort"

	"diffaro/internal/domain"
)

// CandidateGraphs contains independent base/head walks starting from a stored
// Application. Descendants within each walk use their parent's desired specs.
type CandidateGraphs struct {
	Application domain.Application
	Base, Head  Snapshot
}

// SelectRoots removes inventory duplicates using both revisions. Only walks
// belonging to selected roots consume candidates: a stored child's obsolete
// descendants must not hide independent Applications from the final analysis.
// Unresolved cyclic candidates are returned separately rather than guessing.
func SelectRoots(candidates []CandidateGraphs) (roots []domain.Application, unresolved []domain.ApplicationID) {
	remaining := map[domain.ApplicationID]CandidateGraphs{}
	for _, candidate := range candidates {
		remaining[candidate.Application.ID] = candidate
	}
	for len(remaining) > 0 {
		incoming := map[domain.ApplicationID]bool{}
		for id, candidate := range remaining {
			for _, snapshot := range []Snapshot{candidate.Base, candidate.Head} {
				for child := range snapshot.Nodes {
					if child != id {
						if _, exists := remaining[child]; exists {
							incoming[child] = true
						}
					}
				}
			}
		}
		ready := []domain.ApplicationID{}
		for id := range remaining {
			if !incoming[id] {
				ready = append(ready, id)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].String() < ready[j].String() })
		if len(ready) == 0 {
			for id := range remaining {
				unresolved = append(unresolved, id)
			}
			break
		}
		// Select this whole layer before removing shared descendants.
		consumed := map[domain.ApplicationID]bool{}
		for _, id := range ready {
			candidate := remaining[id]
			roots = append(roots, candidate.Application)
			consumed[id] = true
			for _, snapshot := range []Snapshot{candidate.Base, candidate.Head} {
				for child := range snapshot.Nodes {
					consumed[child] = true
				}
			}
		}
		for id := range consumed {
			delete(remaining, id)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID.String() < roots[j].ID.String() })
	sort.Slice(unresolved, func(i, j int) bool { return unresolved[i].String() < unresolved[j].String() })
	return roots, unresolved
}
