package localhelm

import (
	"strings"
	"testing"
)

// Argo CD reads manifests with sigs.k8s.io/yaml, where a repeated mapping key
// keeps its last value; local rendering must not reject such charts.
func TestRepeatedMappingKeysKeepLastValueLikeArgoCD(t *testing.T) {
	const doc = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n  annotations:\n    image-tag: old\n    other: x\n    image-tag: new\ndata: {k: v}\n"
	out, err := trackDocuments("app/templates/config.yaml", doc, fixtureRequest(), map[string]any{}, true)
	if err != nil || len(out) != 1 || !strings.Contains(out[0], `"image-tag":"new"`) || strings.Contains(out[0], `"old"`) {
		t.Fatalf("repeated key not resolved to its last value: %v %v", err, out)
	}
}
