package argopolicy

import (
	"errors"
	"testing"

	"diffaro/internal/domain"
)

func TestAuthorizeSourceNamespacesFollowArgoCDGlob(t *testing.T) {
	destination := map[string]any{"name": "in-cluster", "server": "https://kubernetes.default.svc", "namespace": "demo"}
	for _, tc := range []struct {
		name, namespace string
		rules           any
		code            string
	}{
		{"controller namespace needs no rule", "argocd", nil, ""},
		{"exact", "team-a", []any{"team-a"}, ""},
		{"glob", "team-a", []any{"team-*"}, ""},
		{"single character glob", "team-a", []any{"team-?"}, ""},
		{"wildcard", "anything", []any{"*"}, ""},
		{"glob without match", "other", []any{"team-*"}, "access_denied"},
		{"glob needs suffix", "team", []any{"team-*"}, "access_denied"},
		{"missing rules", "team-a", nil, "access_denied"},
		{"empty rules", "team-a", []any{}, "access_denied"},
		{"regexp unsupported", "team-a", []any{"/^team-.*$/"}, "unsupported_project_policy"},
		{"literal bang unsupported", "team-b", []any{"!team-a"}, "unsupported_project_policy"},
		{"invalid glob unsupported", "team-a", []any{"team-["}, "unsupported_project_policy"},
		{"non-string unsupported", "team-a", []any{map[string]any{}}, "unsupported_project_policy"},
		{"supported match wins over unsupported entry", "team-a", []any{"/^x$/", "!y", "team-*"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := map[string]any{"sourceRepos": []any{"*"}, "destinations": []any{map[string]any{"server": "*", "namespace": "*"}}}
			if tc.rules != nil {
				project["sourceNamespaces"] = tc.rules
			}
			err := Authorize(project, repository, destination, tc.namespace, "argocd")
			var failure *domain.Failure
			if (err == nil) != (tc.code == "") || (err != nil && (!errors.As(err, &failure) || failure.Issue.Code != tc.code)) {
				t.Fatalf("want %q, got %v", tc.code, err)
			}
		})
	}
}
