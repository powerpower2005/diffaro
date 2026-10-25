package httpx

import (
	"context"
	"net/http"
	"net/url"
)

// OpenFor sends a GET for Base+path with the client credential and returns
// the response open, so the caller can stream and bound its body; the caller
// closes it. A 200 or 3xx response is returned. Redirects are not followed:
// the caller validates the Location and calls OpenRedirectFor. Other
// statuses and transport errors become the same failures as GetFor. Unlike
// GetFor, it records no trace; the caller traces the whole download.
func (c *Client) OpenFor(ctx context.Context, path, accept string, request RequestDiagnostic) (*http.Response, error) {
	return c.open(ctx, c.Base+path, true, accept, request)
}

// OpenRedirectFor is OpenFor for a redirect location the caller has
// validated. It never sends the credential: the location may be another host,
// and signed download URLs need none.
func (c *Client) OpenRedirectFor(ctx context.Context, location *url.URL, accept string, request RequestDiagnostic) (*http.Response, error) {
	return c.open(ctx, location.String(), false, accept, request)
}

func (c *Client) open(ctx context.Context, target string, credential bool, accept string, request RequestDiagnostic) (*http.Response, error) {
	resp, err := c.send(ctx, target, credential, accept)
	if err != nil {
		return nil, c.describe(err, request)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp, nil
	}
	if err := c.responseFailure(resp); err != nil {
		resp.Body.Close()
		return nil, c.describe(err, request)
	}
	return resp, nil
}
