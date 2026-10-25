package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"diffaro/internal/domain"
)

// RateLimited reports a provider quota rejection as rate_limited instead of
// access denial or a generic backend error. GitHub and GHES signal exhausted
// quotas with 403 or 429 plus X-RateLimit-Remaining: 0 or Retry-After. It
// returns nil for any other response. Only numeric or HTTP-date header values
// are interpreted; header text is never echoed.
func RateLimited(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	exhausted := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusForbidden && !exhausted && retryAfter == "" {
		return nil
	}
	reason := (&StatusError{StatusCode: resp.StatusCode}).Error() + "; API rate limit or secondary rate limit was reached"
	if reset, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil && reset > 0 {
		reason += "; quota resets at " + time.Unix(reset, 0).UTC().Format(time.RFC3339)
	}
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds >= 0 {
		reason += "; retry after " + strconv.FormatInt(seconds, 10) + "s"
	} else if at, err := http.ParseTime(retryAfter); err == nil {
		reason += "; retry after " + at.UTC().Format(time.RFC3339)
	}
	cause := &StatusError{StatusCode: resp.StatusCode}
	diagnostic := domain.Locate(cause).With(func(d *domain.Diagnostic) { d.HTTPStatus = resp.StatusCode })
	return &domain.Failure{Issue: domain.Issue{Code: domain.CodeRateLimited, Stage: "connection", Reason: reason, Action: "wait for the API quota reset or use a token with a separate quota; reduce concurrent runs sharing this token", Diagnostic: diagnostic}, Cause: cause}
}
