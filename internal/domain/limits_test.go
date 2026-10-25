package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestLimitDiagnosticIdentifiesMeasuredSettingAndHardCeiling(t *testing.T) {
	for _, tc := range []struct {
		name, setting            string
		observed, limit, maximum int64
		lower                    bool
		action                   string
	}{
		{"configurable", "render.maxResponseBytes", 1025, 1024, 64 << 20, true, "increase render.maxResponseBytes"},
		{"maximum", "render.maxDocuments", 10001, 10000, 10000, true, "already at the supported maximum"},
		{"fixed", "revision graph retained-byte budget", 1025, 1024, 0, false, "limit is fixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := LimitError("limit_exceeded", "manifest", "input", tc.setting, tc.observed, tc.limit, "bytes", tc.lower, tc.maximum)
			var failure *Failure
			if !errors.As(err, &failure) || !strings.Contains(failure.Issue.Reason, tc.setting) || !strings.Contains(failure.Issue.Reason, "limit ") || !strings.Contains(failure.Issue.Action, tc.action) {
				t.Fatal("limit diagnostic does not identify the bound", err)
			}
			if strings.Contains(failure.Issue.Reason, "at least") != tc.lower || strings.Contains(failure.Issue.Action, "full size is unknown") != tc.lower {
				t.Fatal("partial measurement was presented as exact")
			}
			// The site is the line that hit the limit.
			if failure.Issue.Diagnostic == nil || !strings.HasPrefix(failure.Issue.Diagnostic.Site, "domain/limits_test.go:") {
				t.Fatalf("site %+v is not the caller", failure.Issue.Diagnostic)
			}
		})
	}
}
