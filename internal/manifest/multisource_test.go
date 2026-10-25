package manifest

import (
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

func TestMultiSourceLastOccurrenceWinsWithoutMerging(t *testing.T) {
	first := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"old":"must disappear"}}`
	last := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"demo"},"data":{"new":"selected"}}`
	limits := Limits{Bytes: 1 << 20, Documents: 10}
	if _, err := Parse([]string{first, last}, "cluster", "demo", limits, nil); err == nil {
		t.Fatal("single-source duplicate stopped being an error")
	}
	for _, raws := range [][]string{{first, last, last}, {last, first}} {
		resources, repeated, err := ParseMultiSource(raws, "cluster", "demo", limits, nil)
		if err != nil || len(resources) != 1 || len(repeated) != 1 {
			t.Fatalf("duplicate handling failed: %v", err)
		}
		want, err := Parse(raws[len(raws)-1:], "cluster", "demo", limits, nil)
		if err != nil || !reflect.DeepEqual(resources, want) {
			t.Fatal("outputs were merged or source order was lost")
		}
		if repeated[0] != (domain.ResourceID{Cluster: "cluster", Kind: "ConfigMap", Namespace: "demo", Name: "settings"}) {
			t.Fatal("namespace-aware duplicate identity lost")
		}
	}
	if _, _, err := ParseMultiSource([]string{first, last}, "cluster", "demo", Limits{Bytes: 1 << 20, Documents: 1}, nil); err == nil {
		t.Fatal("overwritten resource bypassed document budget")
	}
	invalid := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"x":.nan}}`
	if _, _, err := ParseMultiSource([]string{invalid, last}, "cluster", "demo", limits, nil); err == nil {
		t.Fatal("invalid shadowed object was ignored")
	}
}

func TestMultiSourceNamespacesAndVersions(t *testing.T) {
	raw := []string{
		`apiVersion: apps/v1beta1
kind: Deployment
metadata: {name: api, namespace: one}
spec: {replicas: 1}`,
		`apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: one}
spec: {replicas: 2}`,
		`apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: two}
spec: {replicas: 3}`,
	}
	resources, repeated, err := ParseMultiSource(raw, "cluster", "demo", Limits{Bytes: 1 << 20, Documents: 10}, nil)
	if err != nil || len(resources) != 2 || len(repeated) != 1 || repeated[0].Namespace != "one" {
		t.Fatalf("group/kind/namespace identity precedence failed: %v", err)
	}
}
