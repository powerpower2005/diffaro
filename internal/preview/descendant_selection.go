package preview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
)

// descendantSelector extends changed-path selection to rendered children. A child is skipped in both walks only when its parent has the
// same spec on both revisions, the parent renders the child with the same
// spec at base and head, and the child's own inputs are proven unchanged.
// Repository candidates are never skipped, so root selection is unaffected.
type descendantSelector struct {
	paths      *changedPathSelector
	pair       domain.RevisionPair
	manages    map[domain.ApplicationID]bool
	candidates map[domain.ApplicationID]bool

	mu sync.Mutex
	// stable holds nodes whose spec is the same on both revisions.
	stable map[specKey]bool
	// decisions holds one decision per parent/child input, shared by the
	// base and head walks so a transient failure cannot make them diverge.
	decisions flight[decisionKey, bool]
	// reasons keeps the least reason of the skip decisions per parent and
	// child identity.
	reasons map[[2]domain.ApplicationID]string
	// skipped keeps one stable reason per child skipped in the final walks.
	skipped map[domain.ApplicationID]skippedDescendant
}

type specKey struct {
	id   domain.ApplicationID
	spec [32]byte
}

type decisionKey struct {
	parent, child specKey
	root          bool
}

type skippedDescendant struct {
	parent domain.ApplicationID
	reason string
}

func newDescendantSelector(paths *changedPathSelector, pair domain.RevisionPair, manages map[domain.ApplicationID]bool, candidates []domain.Application) *descendantSelector {
	ids := map[domain.ApplicationID]bool{}
	for _, app := range candidates {
		ids[app.ID] = true
	}
	return &descendantSelector{paths: paths, pair: pair, manages: manages, candidates: ids, stable: map[specKey]bool{}, reasons: map[[2]domain.ApplicationID]string{}, skipped: map[domain.ApplicationID]skippedDescendant{}}
}

func digest(app domain.Application) (specKey, bool) {
	data, err := json.Marshal(app.Spec)
	if err != nil {
		return specKey{}, false
	}
	return specKey{id: app.ID, spec: sha256.Sum256(data)}, true
}

// walkSkip returns the Skip hook of one walk of revision, which decides each
// child of a parent seen in that walk. opposite expands the parent at the
// other revision through the run's render cache, which the other walk
// shares. The first walk to reach a parent/child input decides it for both.
// A walk asks about a parent's children in turn from one goroutine, so the
// hook digests each parent spec once for all of them.
func (d *descendantSelector) walkSkip(revision string, opposite expandFunc) func(context.Context, domain.Application, bool, domain.Application) bool {
	var parent domain.Application
	var parentKey specKey
	var parentOK bool
	return func(ctx context.Context, p domain.Application, root bool, child domain.Application) bool {
		if d.candidates[child.ID] || (revision != d.pair.Base && revision != d.pair.Head) {
			return false
		}
		// The same map is equal at once; another spec is digested again.
		if parent.Spec == nil || p.ID != parent.ID || !reflect.DeepEqual(p.Spec, parent.Spec) {
			parent = p
			parentKey, parentOK = digest(p)
		}
		childKey, childOK := digest(child)
		if !parentOK || !childOK {
			return false
		}
		return d.skip(ctx, p, parentKey, root, child, childKey, opposite)
	}
}

func (d *descendantSelector) skip(ctx context.Context, parent domain.Application, parentKey specKey, root bool, child domain.Application, childKey specKey, opposite expandFunc) bool {
	// Every decision is kept, including one made after a transient failure.
	skip, _, err := d.decisions.do(ctx, decisionKey{parent: parentKey, child: childKey, root: root}, func() bool {
		skip, reason := d.decide(ctx, parent, parentKey, root, child, childKey, opposite)
		if skip {
			pair := [2]domain.ApplicationID{parent.ID, child.ID}
			d.mu.Lock()
			if prior, ok := d.reasons[pair]; !ok || reason < prior {
				d.reasons[pair] = reason
			}
			d.mu.Unlock()
		}
		return skip
	}, func(bool) bool { return true })
	return err == nil && skip
}

func (d *descendantSelector) decide(ctx context.Context, parent domain.Application, parentKey specKey, root bool, child domain.Application, childKey specKey, opposite func(context.Context, domain.Application, bool) (graph.Expansion, error)) (bool, string) {
	d.mu.Lock()
	stable := root || d.stable[parentKey]
	d.mu.Unlock()
	if !stable {
		return false, ""
	}
	other, err := opposite(ctx, parent, root)
	if err != nil {
		return false, ""
	}
	same := false
	for _, resource := range other.Resources {
		if resource.ID.Group == "argoproj.io" && resource.ID.Kind == "Application" && resource.ID.Namespace == child.ID.Namespace && resource.ID.Name == child.ID.Name {
			spec, _ := resource.Object["spec"].(map[string]any)
			same = reflect.DeepEqual(spec, child.Spec)
		}
	}
	if !same {
		return false, ""
	}
	d.mu.Lock()
	d.stable[childKey] = true
	d.mu.Unlock()
	var manages *bool
	if value, ok := d.manages[child.ID]; ok {
		manages = &value
	}
	selection, reason := d.paths.decideDescendant(ctx, child, manages)
	if selection != selectionUnaffected {
		return false, ""
	}
	return true, reason
}

// record keeps reasons only for children the final base and head walks
// skipped and neither visited; candidate probe walks never reach the report.
func (d *descendantSelector) record(base, head graph.Snapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, snapshot := range []graph.Snapshot{base, head} {
		for childID, parentID := range snapshot.Skipped {
			if base.Nodes[childID] != nil || head.Nodes[childID] != nil {
				continue
			}
			if prior, ok := d.skipped[childID]; ok && prior.parent.String() <= parentID.String() {
				continue
			}
			reason := d.reasons[[2]domain.ApplicationID{parentID, childID}]
			d.skipped[childID] = skippedDescendant{parent: parentID, reason: "descendant of " + parentID.String() + " with the same spec on both revisions; " + reason}
		}
	}
}

// annotate records skipped descendants in the inventory decisions and counts.
func (d *descendantSelector) annotate(results []domain.DiscoveryResult) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	total := 0
	for i := range results {
		for j := range results[i].Decisions {
			decision := &results[i].Decisions[j]
			// Candidates keep their own decision; an unaffected candidate
			// is already counted as such.
			if skipped, ok := d.skipped[decision.ID]; ok && decision.Selection == "" {
				decision.Selection, decision.SelectionReason = selectionUnaffected, skipped.reason
				results[i].UnaffectedDescendants++
				total++
			}
		}
	}
	return total
}
