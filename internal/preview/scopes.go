package preview

import "diffaro/internal/domain"

// clusterScopes maps "instance:cluster" to the group/kind scopes Argo CD
// reports for readable Applications with that destination. Kinds
// reported with conflicting scopes are omitted.
type clusterScopes map[string]map[string]bool

func aggregateScopes(instance string, apps []domain.Application) clusterScopes {
	out := clusterScopes{}
	conflicts := map[string]map[string]bool{}
	for _, app := range apps {
		cluster, _ := destination(app)
		if cluster == "" || len(app.ObservedScopes) == 0 {
			continue
		}
		key := instance + ":" + cluster
		if out[key] == nil {
			out[key], conflicts[key] = map[string]bool{}, map[string]bool{}
		}
		for kind, clusterScoped := range app.ObservedScopes {
			if previous, seen := out[key][kind]; seen && previous != clusterScoped {
				conflicts[key][kind] = true
			}
			out[key][kind] = clusterScoped
		}
	}
	for key, kinds := range conflicts {
		for kind := range kinds {
			delete(out[key], kind)
		}
	}
	return out
}

// scopesFor returns the Application's own observed scopes, completed by the
// destination cluster's aggregate. The Application's own observation wins.
func (s clusterScopes) scopesFor(app domain.Application, cluster string) map[string]bool {
	aggregate := s[cluster]
	if len(aggregate) == 0 {
		return app.ObservedScopes
	}
	out := make(map[string]bool, len(aggregate)+len(app.ObservedScopes))
	for kind, clusterScoped := range aggregate {
		out[kind] = clusterScoped
	}
	for kind, clusterScoped := range app.ObservedScopes {
		out[kind] = clusterScoped
	}
	return out
}
