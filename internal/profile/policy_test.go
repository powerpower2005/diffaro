package profile

import (
	"strings"
	"testing"
)

func TestPolicyProfile(t *testing.T) {
	block := "policies:\n  - id: protect-services\n    rule: deny-resource-removal-v1\n    match:\n      kind: Service\n      instance: test\n"
	c, err := Parse([]byte(valid + block))
	if err != nil || len(c.Policies) != 1 || c.Policies[0].Match.Group != "" {
		t.Fatalf("valid core policy rejected: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(block, "instance: test", "instance: missing", 1),
		strings.Replace(block, "kind: Service", "kind: Service\n      expression: untrusted", 1),
		strings.Replace(block, "deny-resource-removal-v1", "unknown", 1),
	} {
		if _, err := Parse([]byte(valid + bad)); err == nil {
			t.Fatal("bad policy config accepted")
		}
	}
}
