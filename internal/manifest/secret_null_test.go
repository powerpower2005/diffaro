package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func secretTarget(t *testing.T, data string) map[string]Resource {
	t.Helper()
	resources, err := Parse([]string{"apiVersion: v1\nkind: Secret\nmetadata: {name: s}\n" + data}, "in-cluster", "demo", Limits{Bytes: 1 << 20, Documents: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resources
}

// A null data section, as a template ranging over nothing renders, holds no
// payload: it is not an Argo CD-masked Secret (found by FuzzParseManifests).
func TestNullSecretDataIsComparedNotUnknown(t *testing.T) {
	diffs, _ := Compare(secretTarget(t, "data:\n"), secretTarget(t, "data:\n"), security.Redactor{})
	if len(diffs) != 1 || diffs[0].State != domain.StateUnchanged || diffs[0].Comparison == domain.ComparisonIncomplete {
		t.Fatalf("identical null-data Secrets: %+v", diffs)
	}
	diffs, _ = Compare(secretTarget(t, "data:\n"), secretTarget(t, "data: {key: ZmljdGlvbmFsLXNlY3JldA==}\n"), security.Redactor{})
	if len(diffs) != 1 || diffs[0].State != domain.StateChanged || diffs[0].Comparison == domain.ComparisonIncomplete {
		t.Fatalf("payload added to a null section: %+v", diffs)
	}
	encoded, _ := json.Marshal(diffs)
	if strings.Contains(string(encoded), "ZmljdGlvbmFsLXNlY3JldA==") {
		t.Fatalf("added payload is not masked: %s", encoded)
	}
	diffs, _ = Compare(secretTarget(t, "data: {key: '++++++++'}\n"), secretTarget(t, "data: {key: '++++++++'}\n"), security.Redactor{})
	if diffs[0].IncompleteReason != domain.IncompleteMaskedSecret {
		t.Fatalf("an Argo CD-masked payload is no longer unknown: %+v", diffs)
	}
}
