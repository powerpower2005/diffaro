package profile

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"diffaro/internal/domain"
)

type ParentRoute struct {
	Parent   domain.ApplicationID `yaml:"parent"`
	Instance string               `yaml:"instance"`
}
type ClusterRoute struct {
	ParentInstance string `yaml:"parentInstance,omitempty"`
	Cluster        string `yaml:"cluster"`
	Namespace      string `yaml:"namespace"`
	Instance       string `yaml:"instance"`
}
type RepositoryRoute struct {
	RepositoryURL string `yaml:"repositoryURL"`
	Project       string `yaml:"project"`
	Instance      string `yaml:"instance"`
}
type Routing struct {
	Parents         []ParentRoute     `yaml:"parents"`
	Clusters        []ClusterRoute    `yaml:"clusters"`
	Repositories    []RepositoryRoute `yaml:"repositories"`
	DefaultInstance string            `yaml:"defaultInstance"`
}

func (r Routing) validate(instances map[string]bool) error {
	bad := func() error {
		return fmt.Errorf("invalid configuration: routing requires configured instances and complete selectors")
	}
	if r.DefaultInstance != "" && !instances[r.DefaultInstance] {
		return bad()
	}
	for _, p := range r.Parents {
		if !instances[p.Instance] || !instances[p.Parent.Instance] || !identifier.MatchString(p.Parent.Namespace) || !identifier.MatchString(p.Parent.Name) {
			return bad()
		}
	}
	for _, c := range r.Clusters {
		if !instances[c.Instance] || (c.ParentInstance != "" && !instances[c.ParentInstance]) || c.Cluster == "" || !identifier.MatchString(c.Namespace) {
			return bad()
		}
	}
	for _, p := range r.Repositories {
		if !instances[p.Instance] || !ValidEndpoint(p.RepositoryURL) || p.Project == "" {
			return bad()
		}
	}
	return nil
}

// Route uses the parent's destination: this is where the child Application CR
// is created, which need not be the child's workload destination.
func (r Routing) Route(parent, child domain.Application) (string, string, error) {
	d := parent.Destination()
	cluster := domain.String(d, "server")
	if cluster == "" {
		cluster = domain.String(d, "name")
	}
	levels := [][]string{{}, {}, {}, {}}
	for _, p := range r.Parents {
		if p.Parent == parent.ID {
			levels[0] = append(levels[0], p.Instance)
		}
	}
	for _, c := range r.Clusters {
		if (c.ParentInstance == "" || c.ParentInstance == parent.ID.Instance) && c.Cluster == cluster && c.Namespace == child.ID.Namespace {
			levels[1] = append(levels[1], c.Instance)
		}
	}
	for _, p := range r.Repositories {
		sources, _ := child.Spec["sources"].([]any)
		if len(sources) == 0 {
			sources = []any{child.Source()}
		}
		for _, raw := range sources {
			source, _ := raw.(map[string]any)
			if SameRepository(p.RepositoryURL, domain.String(source, "repoURL")) && p.Project == child.Project() {
				levels[2] = append(levels[2], p.Instance)
			}
		}
	}
	if r.DefaultInstance != "" {
		levels[3] = append(levels[3], r.DefaultInstance)
	}
	for i, matches := range levels {
		if len(matches) == 0 {
			continue
		}
		for _, instance := range matches {
			if instance != matches[0] {
				unique := map[string]bool{}
				for _, candidate := range matches {
					unique[candidate] = true
				}
				candidates := slices.Sorted(maps.Keys(unique))
				setting := []string{"routing.parents", "routing.clusters", "routing.repositories", "routing.defaultInstance"}[i]
				return "", "", routeFailure(parent, child, domain.CodeRoutingAmbiguous, fmt.Sprintf("multiple instances match %s at the same priority: %s", setting, strings.Join(candidates, ", ")), "make matching "+setting+" rules identify one managing instance; a lower-priority default cannot resolve this conflict")
			}
		}
		return matches[0], []string{"parent", "cluster", "repository-project", "explicit-default"}[i], nil
	}
	kind, destination := routingDestination(parent)
	action := "edit routing.parents for the reported parent and set instance=<configured managing instance ID>"
	if kind != "unresolved" && destination != "[unsafe server identifier omitted]" {
		action = fmt.Sprintf("edit routing.clusters: parentInstance=%q, cluster=%q, namespace=%q, instance=<configured ID>", parent.ID.Instance, destination, child.ID.Namespace)
	}
	action += "; only if all descendants have one confirmed manager, set routing.defaultInstance=<that instance ID>; init --single-instance creates that explicit default in a new profile; the child's workload destination does not identify its managing instance"
	return "", "", routeFailure(parent, child, domain.CodeRoutingMissing, "no explicit route identifies the child's managing Argo CD instance", action)
}

// Diagnostics retain only routing selectors, never the rendered spec or server
// credentials. The child instance remains unresolved until a route succeeds.
func routeFailure(parent, child domain.Application, code, reason, action string) error {
	kind, destination := routingDestination(parent)
	reason += fmt.Sprintf("; parent=%q; parent destination %s=%q; child Application=%q; child project=%q", parent.ID.String(), kind, destination, child.ID.Namespace+"/"+child.ID.Name, child.Project())
	return domain.Fail(code, "routing", reason, action, nil)
}

func routingDestination(parent domain.Application) (string, string) {
	d := parent.Destination()
	if server := domain.String(d, "server"); server != "" {
		parsed, err := url.Parse(server)
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return "server", "[unsafe server identifier omitted]"
		}
		return "server", server
	}
	if name := domain.String(d, "name"); name != "" {
		return "name", name
	}
	return "unresolved", ""
}
