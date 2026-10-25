// Package graph discovers target Application graphs without provider clients.
//
// Walker.Walk traverses App-of-Apps children through an injected Expand
// function and enforces depth, Application, cycle and byte limits; a
// Snapshot retains at most MaxFaults faults.
// CompareEdges diffs base and head graphs and SelectRoots picks discovery
// roots. Rendering and revision mapping are injected by package preview.
package graph

import (
	"diffaro/internal/domain"
	"sort"
)

// CompareEdges can prove removal of a direct edge even when an independent
// branch failed. It does not claim that the child is globally absent.
func CompareEdges(base, head Snapshot) []domain.GraphEdgeChange {
	type key struct{ parent, child domain.ApplicationID }
	left, right, union := map[key]bool{}, map[key]bool{}, map[key]bool{}
	for _, edge := range base.Edges {
		k := key{edge.Parent, edge.Child}
		left[k], union[k] = true, true
	}
	for _, edge := range head.Edges {
		k := key{edge.Parent, edge.Child}
		right[k], union[k] = true, true
	}
	keys := []key{}
	for k := range union {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].parent != keys[j].parent {
			return keys[i].parent.String() < keys[j].parent.String()
		}
		return keys[i].child.String() < keys[j].child.String()
	})
	known := func(s Snapshot, parent domain.ApplicationID) bool {
		if n := s.Nodes[parent]; n != nil {
			return n.Discovered
		}
		return s.Complete
	}
	changes := []domain.GraphEdgeChange{}
	for _, k := range keys {
		state := domain.StateUnchanged
		if !left[k] {
			state = domain.StateAdded
			if !known(base, k.parent) {
				state = domain.StateUnknown
			}
		}
		if !right[k] {
			state = domain.StateRemoved
			if !known(head, k.parent) {
				state = domain.StateUnknown
			}
		}
		if (left[k] && (base.Nodes[k.parent] == nil || !base.Nodes[k.parent].Rendered)) || (right[k] && (head.Nodes[k.parent] == nil || !head.Nodes[k.parent].Rendered)) {
			state = domain.StateUnknown
		}
		changes = append(changes, domain.GraphEdgeChange{Parent: k.parent, Child: k.child, State: state})
	}
	return changes
}
