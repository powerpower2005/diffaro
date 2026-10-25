package profile

import (
	"fmt"
	"path/filepath"
)

// Connections is a trusted, reusable server catalog. It carries environment
// references or private tokens and optional render setup, never PR manifests.
type Connections struct {
	Instances []Instance `yaml:"instances"`
	Routing   Routing    `yaml:"routing,omitempty"`
}

func LoadConnections(filename string) (Connections, error) {
	var connections Connections
	data, err := readBounded(filename, "connections", "connections file")
	if err != nil {
		return connections, err
	}
	if err := decodeStrict(data, &connections, "connections"); err != nil {
		return connections, err
	}
	if len(connections.Instances) == 0 {
		return connections, fmt.Errorf("connections require at least one Argo CD instance")
	}
	for _, instance := range connections.Instances {
		if !ValidToken(instance.Token) {
			return connections, fmt.Errorf("connections contain an invalid token")
		}
	}
	if err := resolveInstancePaths(connections.Instances, filepath.Dir(filename)); err != nil {
		return connections, err
	}
	return connections, nil
}
