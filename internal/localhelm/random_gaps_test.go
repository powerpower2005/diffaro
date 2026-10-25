package localhelm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
)

// g67aValueTemplateRequest renders a chart whose only template evaluates a
// value with tpl, so a function name can appear only in the
// Application-supplied values, never in the chart itself.
func g67aValueTemplateRequest(key string) Request {
	req := fixtureRequest()
	req.Repositories[testRepo+"@"+testSHA]["app/templates/config.yaml"] = []byte("# {{ tpl .Values." + key + " . }}\n")
	return req
}

// A random-output function that only Application values supply is still a
// randomness call: differing double renders mark the differing field, not an unexplained nondeterministic backend failure.
func TestExternalRandomFunctionOnlyInApplicationValuesIsMarked(t *testing.T) {
	e := fakeExternal(t)
	req := g67aValueTemplateRequest("generated")
	req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{
		"generated":        "{{ uuidv4 }}",
		"externalTestCase": "nondeterministic",
	}
	docs, err := e.Render(context.Background(), req)
	var failure *domain.Failure
	if errors.As(err, &failure) && failure.Issue.Code == "render_nondeterministic" {
		t.Fatal("randomness from Application values reported as an unexplained nondeterministic render", err)
	}
	if err != nil {
		t.Fatal("randomness from Application values was not rendered with the differing field marked", err)
	}
	targets, err := manifest.Parse(docs, "east", "demo", manifest.Limits{Bytes: 1 << 20, Documents: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := targets[(domain.ResourceID{Cluster: "east", Kind: "ConfigMap", Namespace: "demo", Name: "api"}).String()]
	if replicas := config.Object["data"].(map[string]any)["replicas"]; replicas != domain.RenderUnstable {
		t.Fatalf("replicas = %v, want the differing field marked", replicas)
	}
}

// A time function that only Application inline values supply needs the
// bundled-engine probe, which finds the call through tpl. Without the probe
// the deterministic fake CLI would render it.
func TestExternalStableFunctionOnlyInApplicationValuesIsProbed(t *testing.T) {
	e := fakeExternal(t)
	req := g67aValueTemplateRequest("stamp")
	req.Plan[0].Source["helm"].(map[string]any)["values"] = "stamp: '{{ now }}'\n"
	_, err := e.Render(context.Background(), req)
	var skipped *domain.RenderSkipped
	if !errors.As(err, &skipped) || !strings.Contains(skipped.Reason, "invokes randomness") {
		t.Fatalf("got %v, want the probe's randomness skip for a time function supplied by Application values", err)
	}
}
