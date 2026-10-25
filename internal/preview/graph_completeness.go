package preview

import (
	"diffaro/internal/graph"
	"diffaro/internal/profile"
)

// completeInstances proves absence only where an incomplete subtree cannot
// reach the instance through any configured route. Selectors and precedence are
// deliberately ignored here: an unknown descendant can match any such rule.
// This is a conservative bound, not an inference of the child's manager.
func completeInstances(snapshot graph.Snapshot, routing profile.Routing, instances []profile.Instance) map[string]bool {
	complete := map[string]bool{}
	for _, instance := range instances {
		complete[instance.ID] = true
	}
	for id := range snapshot.Nodes {
		complete[id.Instance] = true
	}
	if snapshot.Complete {
		return complete
	}
	// Walker bounds diagnostic retention. Truncation cannot certify isolation.
	if len(snapshot.Faults) >= graph.MaxFaults {
		return nil
	}
	affected := map[string]bool{}
	queue := []string{}
	add := func(instance string) {
		if !affected[instance] {
			affected[instance] = true
			queue = append(queue, instance)
		}
	}
	for _, fault := range snapshot.Faults {
		if fault.ID.Instance == "" {
			return nil
		}
		add(fault.ID.Instance)
	}
	for id, node := range snapshot.Nodes {
		if node == nil || !node.Discovered {
			if id.Instance == "" {
				return nil
			}
			add(id.Instance)
		}
	}
	if len(queue) == 0 {
		return nil // No scoped explanation for incomplete discovery.
	}
	adjacent := map[string][]string{}
	global := []string{}
	for _, route := range routing.Parents {
		adjacent[route.Parent.Instance] = append(adjacent[route.Parent.Instance], route.Instance)
	}
	for _, route := range routing.Clusters {
		if route.ParentInstance == "" {
			global = append(global, route.Instance)
		} else {
			adjacent[route.ParentInstance] = append(adjacent[route.ParentInstance], route.Instance)
		}
	}
	for _, route := range routing.Repositories {
		global = append(global, route.Instance)
	}
	if routing.DefaultInstance != "" {
		global = append(global, routing.DefaultInstance)
	}
	for _, edge := range snapshot.Edges {
		adjacent[edge.Parent.Instance] = append(adjacent[edge.Parent.Instance], edge.Child.Instance)
	}
	for _, instance := range global {
		add(instance)
	}
	for index := 0; index < len(queue); index++ {
		for _, instance := range adjacent[queue[index]] {
			add(instance)
		}
	}
	for instance := range affected {
		complete[instance] = false
	}
	return complete
}
