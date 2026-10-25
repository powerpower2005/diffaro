package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

func appendPolicy(t *testing.T, file string) {
	t.Helper()
	appendProfile(t, file, "policies:\n  - id: keep-api\n    rule: deny-resource-removal-v1\n    match:\n      group: apps\n      kind: Deployment\n      namespace: demo\n")
}

func TestPolicyTargetRemovalCLI(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{remove: true})
	file := configFile(t, g.URL, a.URL)
	appendPolicy(t, file)
	r, code := runJSON(t, file, "--pr", "12")
	if code != 2 || r.Status != "policy_violation" || r.Policy == nil || r.Policy.Evaluation != "complete" || len(r.Policy.Violations) != 1 || r.Revisions.Base != ancestorSHA {
		t.Fatalf("PR policy contract failed: code=%d report=%+v", code, r)
	}
	v := r.Policy.Violations[0]
	if v.Base.Revision != ancestorSHA || v.Head.Revision != headSHA || v.Resource.Name != "api" || !v.Evidence.Before || v.Evidence.After {
		t.Fatalf("policy evidence lost: %+v", v)
	}
	for _, format := range []string{"json", "markdown", "pr-markdown"} {
		data, err := reporter.Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("Comparison is incomplete or denied")) {
			t.Fatal("complete policy violation labeled as render failure")
		}
	}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--render", "--config", file, "--base", "main", "--head", "feature", "--format", "json"}, &out, &errOut); code != 1 || strings.Contains(out.String(), `"policy":`) {
		t.Fatalf("doctor must not execute diff policy: %d %s", code, errOut.String())
	}
}

func TestPolicyIndependentFailureAndUnchangedCLI(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{remove: true})
	b, _ := argoServer(t, "fictional-argo-1", backendOptions{headFailure: true})
	file := configFile(t, g.URL, a.URL, b.URL)
	appendPolicy(t, file)
	r, code := runJSON(t, file, "--base", "main", "--head", "feature")
	if code != 3 || r.Policy == nil || r.Policy.Evaluation != "incomplete" || len(r.Policy.Violations) != 1 || r.Policy.Violations[0].Application.Instance != "instance-0" {
		t.Fatal("failure masked or independent violation lost")
	}
	r, code = runJSON(t, file, "--base", "main", "--head", "main")
	if code != 0 || r.Policy == nil || len(r.Policy.Violations) != 0 || r.Policy.Evaluation != "complete" {
		t.Fatal("unchanged pair produced a policy violation")
	}
}

func TestPolicyRemovedChildCLI(t *testing.T) {
	g := gitServer(t)
	a, _ := recursiveServer(t, "removed", "fictional-argo-0", false, false)
	file := configFile(t, g.URL, a.URL)
	appendProfile(t, file, "routing:\n  defaultInstance: instance-0\n")
	appendPolicy(t, file)
	r, code := runJSON(t, file, "--recursive", "--base", "main", "--head", "feature")
	if code != 2 || r.Policy == nil || len(r.Policy.Violations) != 1 || r.Policy.Violations[0].Application.Name != "child" || !r.Policy.Violations[0].Head.Absent {
		t.Fatalf("removed child policy failed: code=%d policy=%+v", code, r.Policy)
	}
}
