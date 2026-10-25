// Package onboarding builds validated starter profiles for diffaro init from
// verified inputs: explicit Applications (Profile), repository discovery
// (DiscoveryProfile) and connection catalogs (ConnectionsProfile). It keeps
// the given connection as is and never copies Application render values;
// init adds its options, saves effective credentials and writes the private
// profile.
package onboarding

import (
	"fmt"

	"diffaro/internal/profile"
)

func ConnectionsProfile(connections profile.Connections, prURL, gitAPI, gitTokenEnv string, localChildren bool, childNamespace string, limits profile.Render) (profile.Config, error) {
	if len(connections.Instances) == 0 {
		return profile.Config{}, fmt.Errorf("provide at least one Argo CD instance")
	}
	c, err := DiscoveryProfile(connections.Instances[0], prURL, gitAPI, gitTokenEnv, false)
	if err != nil {
		return profile.Config{}, err
	}
	c.Instances, c.Routing, c.Render = connections.Instances, connections.Routing, limits
	if localChildren {
		// This is an explicit user declaration, not inference from inventory or
		// a child's workload destination. Other destinations remain unrouted.
		for _, instance := range c.Instances {
			for _, cluster := range []string{"in-cluster", "https://kubernetes.default.svc"} {
				c.Routing.Clusters = append(c.Routing.Clusters, profile.ClusterRoute{ParentInstance: instance.ID, Cluster: cluster, Namespace: childNamespace, Instance: instance.ID})
			}
		}
	}
	return c, c.Validate()
}
