package domain

import (
	"fmt"
	"sort"
	"strings"
)

type ApplicationID struct {
	Instance  string `json:"instance" yaml:"instance"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Name      string `json:"name" yaml:"name"`
}

func (id ApplicationID) String() string { return id.Instance + "/" + id.Namespace + "/" + id.Name }

// SortApplicationIDs sorts ids by their String form, the order in which
// Applications are compared and reported.
func SortApplicationIDs(ids []ApplicationID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}

// JoinGraphPath renders an Application graph path with the caller's
// output-specific separator.
func JoinGraphPath(path []ApplicationID, separator string) string {
	parts := make([]string, 0, len(path))
	for _, id := range path {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, separator)
}

type ResourceID struct {
	Cluster   string `json:"cluster"`
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (id ResourceID) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", id.Cluster, id.Group, id.Kind, id.Namespace, id.Name)
}
