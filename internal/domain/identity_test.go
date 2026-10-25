package domain

import (
	"slices"
	"testing"
)

// Applications are ordered by their "instance/namespace/name" form, so an
// instance name that sorts after a shorter prefix field by field ("a-b"
// after "a") still sorts by the joined text ('-' before '/').
func TestSortApplicationIDsUsesStringOrder(t *testing.T) {
	ids := []ApplicationID{{Instance: "a", Namespace: "argocd", Name: "x"}, {Instance: "a-b", Namespace: "argocd", Name: "x"}, {Instance: "a", Namespace: "apps", Name: "y"}}
	SortApplicationIDs(ids)
	var got []string
	for _, id := range ids {
		got = append(got, id.String())
	}
	if want := []string{"a-b/argocd/x", "a/apps/y", "a/argocd/x"}; !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
}
