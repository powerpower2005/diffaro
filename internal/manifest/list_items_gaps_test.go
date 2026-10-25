package manifest

import (
	"fmt"
	"testing"

	"diffaro/internal/security"
)

// A shared item whose base and head indexes differ is compared field by field
// at its head index, while a removed item keeps its base index.
func TestChangedSharedItemUsesHeadIndex(t *testing.T) {
	changes := listChanges(t,
		`[{name: sidecar, image: proxy:v1}, {name: app, image: app:v1}]`,
		`[{name: app, image: app:v2}]`,
		security.Redactor{})
	if len(changes) != 2 {
		t.Fatalf("want one image replace and one removed container, got:\n%s", describe(changes))
	}
	replace, remove := changes[0], changes[1]
	if replace.Operation != "replace" || replace.Path != "/spec/template/spec/containers/0/image" || fmt.Sprint(replace.ItemNames) != "[app]" || replace.Before != "app:v1" || replace.After != "app:v2" {
		t.Fatalf("changed shared item not compared at its head index:\n%s", describe(changes))
	}
	if remove.Operation != "remove" || remove.Path != "/spec/template/spec/containers/0" || fmt.Sprint(remove.ItemNames) != "[sidecar]" {
		t.Fatalf("removed item not indexed in the base list:\n%s", describe(changes))
	}
}
