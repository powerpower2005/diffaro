package preview

import (
	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

// sidePresence is what one side's graph proves about a node it lacks.
type sidePresence struct {
	// complete: discovery is complete for the node's instance, so the node
	// is absent on this side.
	complete bool
	// skipped: changed-path selection skipped the node or an ancestor on
	// its path on this side, so it is not proven absent.
	skipped bool
	// boundaryOnly: the graph is incomplete only through skipped subtrees,
	// without discovery faults.
	boundaryOnly bool
	// incompleteAncestor: the deepest ancestor on the node's path that this
	// side has but could not discover the children of, when there is one.
	incompleteAncestor *domain.ApplicationID
}

// presence is what the base and head graphs prove about one node.
type presence struct {
	base, head sidePresence
}

// nodePresence derives the presence of id from both snapshots and the
// instances each snapshot proves complete.
func nodePresence(base, head graph.Snapshot, baseInstances, headInstances map[string]bool, id domain.ApplicationID) presence {
	return presence{
		base: sidePresence{complete: base.Complete || baseInstances[id.Instance], skipped: skippedOn(base, head.Nodes[id]), boundaryOnly: !base.Complete && len(base.Faults) == 0, incompleteAncestor: undiscoveredAncestor(base, head.Nodes[id])},
		head: sidePresence{complete: head.Complete || headInstances[id.Instance], skipped: skippedOn(head, base.Nodes[id]), boundaryOnly: !head.Complete && len(head.Faults) == 0, incompleteAncestor: undiscoveredAncestor(head, base.Nodes[id])},
	}
}

// undiscoveredAncestor returns the deepest ancestor on the path of a node
// that other found whose node s has without discovered children, such as a
// parent whose render failed there; nil when there is none.
func undiscoveredAncestor(s graph.Snapshot, other *graph.Node) *domain.ApplicationID {
	if other == nil {
		return nil
	}
	for i := len(other.Path) - 2; i >= 0; i-- {
		if node := s.Nodes[other.Path[i]]; node != nil && !node.Discovered {
			id := other.Path[i]
			return &id
		}
	}
	return nil
}

// skippedOn reports whether a node that other found is missing from s because
// descendant selection skipped it or an ancestor on its path there.
func skippedOn(s graph.Snapshot, other *graph.Node) bool {
	if other == nil {
		return false
	}
	for _, id := range other.Path {
		if _, ok := s.Skipped[id]; ok {
			return true
		}
	}
	return false
}

// compareNode compares one Application's base and head graph nodes; at most
// one of them is nil, and p states what the side lacking it proves. Stages:
// nodeResult → applyPresence → spec changes (applySpecChanges) when neither
// node is ambiguous → markSideFailures → applyNodeSkip → resource diff
// (applyNodeResources) when both sides rendered or the absence is known.
func compareNode(b, h *graph.Node, p presence, redactor security.Redactor) domain.ApplicationResult {
	out := nodeResult(b, h)
	knownAbsence := applyPresence(&out, b, h, p)
	if b != nil && h != nil && !b.Ambiguous && !h.Ambiguous {
		applySpecChanges(&out, b, h, redactor)
	}
	markSideFailures(&out, nodeFailure("base", b), nodeFailure("head", h))
	applyNodeSkip(&out, b, h)
	if (b == nil || b.Rendered) && (h == nil || h.Rendered) && ((b != nil && h != nil) || knownAbsence) {
		applyNodeResources(&out, b, h, redactor)
	}
	return out
}

// presentNode is the head node, or the base node when head lacks it.
func presentNode(b, h *graph.Node) *graph.Node {
	if h == nil {
		return b
	}
	return h
}

// nodeResult is the unchanged, complete result of the present node with
// both sides' target summaries and any placeholder input.
func nodeResult(b, h *graph.Node) domain.ApplicationResult {
	n := presentNode(b, h)
	t := target(n)
	out := domain.ApplicationResult{ID: n.Application.ID, Root: n.Root, GraphPath: n.Path, Project: t.Project, Cluster: t.Cluster, Namespace: t.Namespace, Inputs: t.Inputs, State: domain.StateUnchanged, Execution: domain.ExecutionComplete, Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{}, BaseTarget: target(b), HeadTarget: target(h)}
	for _, node := range []*graph.Node{b, h} {
		if node != nil && node.Placeholder != nil {
			out.PlaceholderInput = node.Placeholder
		}
	}
	return out
}

// applyPresence states a node one side lacks as added, removed or, when
// that side does not prove its absence, unknown. It reports whether the
// absence is known.
func applyPresence(out *domain.ApplicationResult, b, h *graph.Node, p presence) bool {
	n := presentNode(b, h)
	// missing is what the side lacking the node proves.
	missing := p.base
	if h == nil {
		missing = p.head
	}
	// A node skipped on one side only is not proven absent there.
	oneSided := (b == nil || h == nil) && missing.skipped
	knownAbsence := !oneSided && (b == nil || h == nil) && missing.complete
	if b == nil {
		out.State = domain.StateAdded
	} else if h == nil {
		out.State = domain.StateRemoved
	}
	switch {
	case oneSided:
		out.State, out.Execution = domain.StateUnknown, domain.ExecutionSkipped
		out.SkipReason = "Changed-path selection skipped this Application or an ancestor on one revision only; its presence and resource changes are unknown."
	case (b == nil || h == nil) && !knownAbsence && missing.boundaryOnly && n.Err == nil:
		// Skipped subtrees cannot prove absence elsewhere in the graph. Keep
		// presence unknown without turning an analysis boundary into failure.
		out.State, out.Execution = domain.StateUnknown, domain.ExecutionSkipped
		out.SkipReason = "Application presence and resource changes are unknown because the opposite graph contains skipped subtrees. Review the configured backend's recorded analysis boundary."
	case (b == nil || h == nil) && !knownAbsence:
		out.State, out.Execution = domain.StateUnknown, domain.ExecutionFailed
		reason, action := "incomplete opposite graph cannot prove this Application is absent", "resolve graph discovery failures and rerun"
		if a := missing.incompleteAncestor; a != nil {
			// Name the parent whose failure hides this Application.
			reason += "; its ancestor " + a.String() + " was not discovered on that revision"
			action = "resolve the failure of " + a.String() + " and rerun"
		}
		out.Issues = append(out.Issues, ContextualIssue(domain.Fail(domain.CodeGraphPresenceUnknown, "graph", reason, action, nil), out.ID, n.Repository, n.Revision))
	}
	return knownAbsence
}

// applySpecChanges compares the Application specs and inline values of both
// nodes; a changed spec changes the Application.
func applySpecChanges(out *domain.ApplicationResult, b, h *graph.Node, redactor security.Redactor) {
	key := domain.ResourceID{Kind: "Application", Name: out.ID.Name}
	wrap := func(node *graph.Node) map[string]manifest.Resource {
		// Compare needs JSON-normalized objects, as Parse returns.
		return map[string]manifest.Resource{key.String(): {ID: key, Object: domain.CloneSpec(map[string]any{"spec": node.Application.Spec})}}
	}
	diff, _ := manifest.Compare(wrap(b), wrap(h), redactor)
	out.SpecChanges = diff[0].Changes
	if len(out.SpecChanges) > 0 {
		out.State = domain.StateChanged
	}
	out.InputChanges = inlineValueChanges(b.Application, h.Application, redactor)
}

// applyNodeSkip skips a complete result whose node skipped rendering on
// either side; an unchanged one becomes unknown.
func applyNodeSkip(out *domain.ApplicationResult, b, h *graph.Node) {
	if out.Execution != domain.ExecutionComplete {
		return
	}
	for _, node := range []*graph.Node{b, h} {
		if node != nil && node.SkipReason != "" {
			out.Execution, out.SkipReason = domain.ExecutionSkipped, node.SkipReason
		}
	}
	if out.Execution == domain.ExecutionSkipped && out.State == domain.StateUnchanged {
		out.State = domain.StateUnknown
	}
}

// applyNodeResources compares the rendered targets of both nodes, a lacking
// node having none, and lists a removed Application's targets as pruning
// candidates.
func applyNodeResources(out *domain.ApplicationResult, b, h *graph.Node, redactor security.Redactor) {
	left, right := map[string]manifest.Resource{}, map[string]manifest.Resource{}
	if b != nil {
		left = b.Resources
	}
	if h != nil {
		right = h.Resources
	}
	applyResourceDiff(out, left, right, b == nil || b.Err == nil, h == nil || h.Err == nil, redactor)
	if out.State == domain.StateRemoved {
		out.Pruning = &domain.PruningImpact{Certainty: "conditional", Reason: "base targets only; live deletion depends on parent prune policy, child finalizers and cascade behavior", Resources: []domain.ResourceID{}}
		for _, resource := range out.Resources {
			out.Pruning.Resources = append(out.Pruning.Resources, resource.ID)
		}
	}
}

// nodeFailure is the failure of one side's graph node, if any.
func nodeFailure(name string, n *graph.Node) failedSide {
	if n == nil {
		return failedSide{name: name}
	}
	return failedSide{name: name, repository: n.Repository, revision: n.Revision, err: n.Err}
}

// applyResourceDiff sets out.Resources to the diff of the base and head
// targets, each complete when its side rendered without error, and derives
// out.State from it: a changed resource changes an unchanged or unknown
// Application, and an incomplete comparison leaves an unchanged one unknown.
func applyResourceDiff(out *domain.ApplicationResult, base, head map[string]manifest.Resource, baseOK, headOK bool, redactor security.Redactor) {
	var complete bool
	out.Resources, complete = manifest.ComparePartial(base, head, redactor, baseOK, headOK)
	// Targets rendered from a placeholder input are not proven unchanged; a
	// value derived from it cannot be found.
	if out.PlaceholderInput != nil {
		for i := range out.Resources {
			out.Resources[i].Comparison = domain.ComparisonIncomplete
			if out.Resources[i].State == domain.StateUnchanged {
				out.Resources[i].State = domain.StateUnknown
			}
			complete = false
		}
	}
	for _, resource := range out.Resources {
		if (out.State == domain.StateUnchanged || out.State == domain.StateUnknown) && resource.State.IsChange() {
			out.State = domain.StateChanged
		}
	}
	// Secrets whose values Argo CD masked stay unknown resources; they are
	// not a failure of the Application.
	if !complete && out.State == domain.StateUnchanged {
		out.State = domain.StateUnknown
	}
}

// target summarizes one side's graph node; nil when the side lacks it.
func target(n *graph.Node) *domain.TargetSummary {
	if n == nil {
		return nil
	}
	cluster, namespace := destination(n.Application)
	inputs := inputSummary(n.Application.Source())
	inputs.IgnoreDifferences = n.Application.IgnoreRuleCount()
	if len(n.Path) > 1 {
		inputs.Origin = "parent-target-manifest"
	}
	execution := domain.ExecutionComplete
	if !n.Rendered || n.Err != nil {
		execution = domain.ExecutionFailed
	}
	if n.SkipReason != "" {
		execution = domain.ExecutionSkipped
	}
	if n.Err != nil && executionFor(n.Err) == domain.ExecutionDenied {
		execution = domain.ExecutionDenied
	}
	return &domain.TargetSummary{SkipReason: n.SkipReason, RepeatedResources: n.RepeatedResources, Sources: n.Sources, Repository: n.Repository, Revision: n.Revision, RevisionRule: n.RevisionRule, RequestedRevision: n.RequestedRevision, Execution: execution, Project: n.Application.Project(), Cluster: cluster, Namespace: namespace, Inputs: inputs, Root: n.Root, GraphPath: n.Path}
}
