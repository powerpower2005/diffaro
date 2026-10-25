package reporter

import (
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Commands validate --format against Formats before contacting a provider,
// so the list must name exactly the formats Encode accepts.
func TestFormatsAreTheFormatsEncodeAccepts(t *testing.T) {
	for _, format := range Formats {
		if _, err := Encode(domain.NewReport(), format, security.Redactor{}); err != nil {
			t.Fatalf("listed format %q rejected: %v", format, err)
		}
	}
	_, err := Encode(domain.NewReport(), "yaml", security.Redactor{})
	if err == nil {
		t.Fatal("unlisted format accepted")
	}
	// The error names the listed formats as the CLI does.
	if want := "format must be json, markdown, pr-markdown or terminal"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
