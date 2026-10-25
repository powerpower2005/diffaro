package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// g71PodChanges compares two Deployments whose pod template spec is the given
// YAML, indented under spec.template.spec.
func g71PodChanges(t *testing.T, base, head string) []domain.Change {
	t.Helper()
	pod := func(spec string) string {
		return deployment + "  template:\n    spec:\n" + spec
	}
	diff, _ := Compare(parsed(t, pod(base)), parsed(t, pod(head)), security.Redactor{})
	if len(diff) != 1 {
		t.Fatalf("got %d resource diffs", len(diff))
	}
	return diff[0].Changes
}

// A removed item has only a before value. When redaction hides part of it,
// the change is marked redacted and the literal value stays masked.
func TestRemovedSensitiveEnvItemIsRedacted(t *testing.T) {
	const keep = "      containers: [{name: app, image: app:v1, env: [{name: MODE, value: fast}"
	changes := g71PodChanges(t, keep+", {name: DB_PASSWORD, value: fictional-db-literal}]}]\n", keep+"]}]\n")
	if len(changes) != 1 {
		t.Fatalf("changes:\n%s", describe(changes))
	}
	c := changes[0]
	if c.Operation != "remove" || c.After != nil || !c.Redacted {
		t.Fatalf("removed sensitive item not marked redacted:\n%s", describe(changes))
	}
	before, _ := json.Marshal(c.Before)
	if strings.Contains(string(before), "fictional-db-literal") || !strings.Contains(string(before), security.Mask) || !strings.Contains(string(before), "DB_PASSWORD") {
		t.Fatalf("removed value not masked or name hidden: %s", before)
	}
}

// A change is redacted only when redaction hid part of a value, not because
// its path is sensitive: an added Secret reference keeps its names visible
// and is not marked redacted.
func TestAddedSecretReferenceUnderSensitivePathIsNotRedacted(t *testing.T) {
	const containers = "      containers: [{name: app, image: app:v1}]\n"
	for _, tc := range []struct {
		name, base, head, path, want string
	}{
		{"imagePullSecrets", containers, containers + "      imagePullSecrets: [{name: registry-pull}]\n", "/spec/template/spec/imagePullSecrets", "registry-pull"},
		{"secret volume", containers + "      volumes: [{name: tls, configMap: {name: web-config}}]\n", containers + "      volumes: [{name: tls, secret: {secretName: web-tls}}]\n", "/spec/template/spec/volumes/0/secret", "web-tls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !(security.Redactor{}).Sensitive(tc.path) {
				t.Fatalf("precondition: %s is not a sensitive path", tc.path)
			}
			changes := g71PodChanges(t, tc.base, tc.head)
			var found bool
			for _, c := range changes {
				if c.Path != tc.path {
					continue
				}
				found = true
				after, _ := json.Marshal(c.After)
				if c.Operation != "add" || c.Redacted || !strings.Contains(string(after), tc.want) {
					t.Fatalf("reference change redacted or hidden:\n%s", describe(changes))
				}
			}
			if !found {
				t.Fatalf("no change at %s:\n%s", tc.path, describe(changes))
			}
		})
	}
}
