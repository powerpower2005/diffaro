package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// GitHub's 422 for a reference naming no commit carries revision_missing in
// the JSON issue; a server error carries server_error instead.
func TestUnresolvableRefReasonCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnprocessableEntity, "revision_missing"},
		{http.StatusInternalServerError, "server_error"},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, `{"message":"No commit found for SHA: deleted-branch"}`)
		}))
		c := Client{HTTP: httpx.New(s.URL, "", time.Second, 1024)}
		_, err := c.Resolve(context.Background(), "example/charts", "deleted-branch")
		s.Close()
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.ReasonCode != tc.want {
			t.Fatalf("status %d: reasonCode of %v, want %q", tc.status, err, tc.want)
		}
		data, _ := json.Marshal(failure.Issue)
		if !strings.Contains(string(data), `"reasonCode":"`+tc.want+`"`) {
			t.Fatalf("reason code missing from JSON issue: %s", data)
		}
	}
}
