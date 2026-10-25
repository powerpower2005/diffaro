package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRemovedPublicationFlagsFailBeforeConfiguration(t *testing.T) {
	for _, flag := range []string{"--publish-comment", "--publish-check", "--publish-token-env=PUBLISH_TOKEN"} {
		var out, diagnostic bytes.Buffer
		code := Run(context.Background(), []string{"diff", "--config", "does-not-exist.yaml", "--pr", "12", flag}, &out, &diagnostic)
		if code != 4 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "invalid arguments") {
			t.Fatalf("removed publication flag was not rejected before any provider call: %s", flag)
		}
	}
}
