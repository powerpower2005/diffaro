package manifest

import (
	"testing"

	"diffaro/internal/security"
)

func TestParseValuesAcceptsHelmYAMLAndRejectsUnsafeKeys(t *testing.T) {
	values, err := ParseValues([]byte("base: &base\n  replicas: 2\nprod:\n  <<: *base\n  tier: web\n---\nignored: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	prod, _ := values["prod"].(map[string]any)
	if prod["replicas"] != 2 || prod["tier"] != "web" || values["ignored"] != nil {
		t.Fatalf("anchors and merge keys resolve in the first document only: %v", values)
	}
	if empty, err := ParseValues(nil); err != nil || len(empty) != 0 {
		t.Fatal("an empty values file is an empty map")
	}
	for _, input := range []string{"? [a]\n: b\n", "1: one\n", "- a\n", "a: [\n"} {
		if _, err := ParseValues([]byte(input)); err == nil {
			t.Fatalf("%q must be rejected", input)
		}
	}
}

func TestCompareValuesUsesPrefixOnlyForRedaction(t *testing.T) {
	base := map[string]any{"image": map[string]any{"tag": "v1"}, "apiToken": "a", "old": 1}
	head := map[string]any{"image": map[string]any{"tag": "v2"}, "apiToken": "b"}
	changes := CompareValues("", base, head, security.Redactor{})
	if len(changes) != 3 || changes[0].Path != "/apiToken" || changes[0].After != security.Mask || changes[1].Path != "/image/tag" || changes[1].After != "v2" || changes[2].Path != "/old" || changes[2].Operation != "remove" {
		t.Fatalf("key-level changes in key order with sensitive keys masked: %+v", changes)
	}
	masked := CompareValues("/spec/source/helm/values", base, head, security.Redactor{})
	if masked[1].Path != "/image/tag" || masked[1].After != security.Mask {
		t.Fatalf("paths are relative to the document; the prefix decides masking: %+v", masked)
	}
}

// A value file list under a sensitive key, such as externalSecrets, is
// compared item by item: the added remoteRef item is one change whose
// references are shown, and keys that may hold credentials stay masked.
func TestValueFileSecretListsAreComparedByItem(t *testing.T) {
	entry := func(refs ...any) map[string]any {
		return map[string]any{"externalSecrets": []any{map[string]any{"name": "gateway-secrets", "type": "data", "remoteRef": refs}}}
	}
	ref := func(name string) any {
		return map[string]any{"key": "gateway/dev", "secretKey": name, "property": name}
	}
	base := entry(ref("AUTH_TOKENS"))
	head := entry(ref("AUTH_TOKENS"), ref("SLACK_WEBHOOK_URL"))
	head["slack"] = map[string]any{"enabled": true, "secretName": "gateway-secrets", "secretKey": "fictional-key"}
	changes := CompareValues("", base, head, security.Redactor{}.ForValueFiles())
	if len(changes) != 2 {
		t.Fatalf("changes: %+v", changes)
	}
	added, slack := changes[0], changes[1]
	after, _ := added.After.(map[string]any)
	if added.Path != "/externalSecrets/0/remoteRef/1" || added.Operation != "add" || len(added.ItemNames) != 2 || added.ItemNames[1] != "SLACK_WEBHOOK_URL" || after["key"] != "gateway/dev" || after["property"] != "SLACK_WEBHOOK_URL" || after["secretKey"] != "SLACK_WEBHOOK_URL" {
		t.Fatalf("added remoteRef item not shown as one item: %+v", added)
	}
	shown, _ := slack.After.(map[string]any)
	if slack.Path != "/slack" || shown["secretName"] != "gateway-secrets" || shown["secretKey"] != security.Mask || !slack.Redacted {
		t.Fatalf("slack keys: %+v", slack)
	}
	// An item with a sensitive name in a list that is not sensitive still
	// stays masked whole, since its field paths would not carry the name.
	env := func(v string) map[string]any {
		return map[string]any{"env": []any{map[string]any{"name": "DB_PASSWORD", "value": v}}}
	}
	if masked := CompareValues("", env("fictional-a"), env("fictional-b"), security.Redactor{}.ForValueFiles()); len(masked) != 1 || masked[0].Path != "/env/0" || masked[0].After.(map[string]any)["value"] != security.Mask {
		t.Fatalf("a sensitive env item leaked: %+v", masked)
	}
	// A redactor for manifests keeps the whole list masked.
	if masked := CompareValues("", base, head, security.Redactor{}); len(masked) != 2 || masked[0].Path != "/externalSecrets" || masked[0].After != security.Mask {
		t.Fatalf("manifest redactor walked a sensitive list: %+v", masked)
	}
}
