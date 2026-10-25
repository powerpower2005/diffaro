package manifest

import (
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

const ackRole = "apiVersion: iam.services.k8s.aws/v1alpha1\nkind: Role\nmetadata: {name: ack-role}\nspec: {name: ack-role}\n"

func observedParse(raw string, observed map[string]bool) (map[string]Resource, error) {
	return Parse([]string{raw}, "c", "demo", Limits{Bytes: 1 << 20, Documents: 100}, observed)
}

func TestObservedScopeInfersMissingNamespace(t *testing.T) {
	for _, tt := range []struct {
		name, raw, namespace string
		observed             map[string]bool
		marked               bool
	}{
		{"namespaced", ackRole, "demo", map[string]bool{"iam.services.k8s.aws/Role": false}, true},
		{"cluster", ackRole, "", map[string]bool{"iam.services.k8s.aws/Role": true}, true},
		{"explicit namespace ignores observed cluster scope", strings.Replace(ackRole, "{name: ack-role}", "{name: ack-role, namespace: team}", 1), "team", map[string]bool{"iam.services.k8s.aws/Role": true}, false},
		{"rendered CRD wins", ackRole + "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: roles.iam.services.k8s.aws}\nspec: {group: iam.services.k8s.aws, scope: Cluster, names: {kind: Role}}\n", "", map[string]bool{"iam.services.k8s.aws/Role": false}, false},
		{"built-in wins", deployment, "demo", map[string]bool{"apps/Deployment": true}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resources, err := observedParse(tt.raw, tt.observed)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range resources {
				if r.ID.Kind == "Role" || r.ID.Kind == "Deployment" {
					found = true
					if r.ID.Namespace != tt.namespace || r.ObservedScope != tt.marked {
						t.Fatalf("got namespace %q observed %v", r.ID.Namespace, r.ObservedScope)
					}
				}
			}
			if !found {
				t.Fatal("resource missing")
			}
		})
	}
}

func TestUnknownScopeFailureNamesResourceAndAction(t *testing.T) {
	for _, observed := range []map[string]bool{nil, {"iam.services.k8s.aws/Policy": false}} {
		_, err := observedParse(ackRole, observed)
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != "scope_unavailable" {
			t.Fatalf("unknown scope accepted: %v", err)
		}
		if !strings.Contains(failure.Issue.Reason, "iam.services.k8s.aws/v1alpha1 Role ack-role") || !strings.Contains(failure.Issue.Action, "metadata.namespace") || !strings.Contains(failure.Issue.Action, "repo-server backend does not change") {
			t.Fatalf("failure is not actionable: %+v", failure.Issue)
		}
	}
	_, err := Parse([]string{ackRole}, "c", "", Limits{Bytes: 1 << 20, Documents: 100}, map[string]bool{"iam.services.k8s.aws/Role": false})
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "scope_unavailable" || !strings.Contains(failure.Issue.Reason, "no destination namespace") {
		t.Fatalf("missing destination namespace accepted: %v", err)
	}
}

func TestObservedScopeIsReportedOnDiff(t *testing.T) {
	observed := map[string]bool{"iam.services.k8s.aws/Role": false}
	b, err := observedParse(ackRole, observed)
	if err != nil {
		t.Fatal(err)
	}
	h, err := observedParse(strings.Replace(ackRole, "spec: {name: ack-role}", "spec: {name: ack-role, description: changed}", 1), observed)
	if err != nil {
		t.Fatal(err)
	}
	diff, _ := Compare(b, h, security.Redactor{})
	if len(diff) != 1 || diff[0].State != "changed" || diff[0].ScopeSource != "argocd-observed" {
		t.Fatalf("observed scope not disclosed: %+v", diff)
	}
	plain, _ := Compare(parsed(t, deployment), parsed(t, deployment), security.Redactor{})
	if plain[0].ScopeSource != "" {
		t.Fatal("target-evidenced scope marked as observed")
	}
}
