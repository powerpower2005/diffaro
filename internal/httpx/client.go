// Package httpx is the bounded HTTP client shared by provider adapters. It
// limits response size and time, never forwards credentials across redirects
// and turns HTTP status codes into domain.Failure values with access
// diagnostics (GetFor, and OpenFor for bodies the caller streams).
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"diffaro/internal/domain"
)

type Client struct {
	Base     string
	Token    string
	HTTP     *http.Client
	Limit    int64
	TokenEnv string
}

// transport is shared by every Client so parallel requests to one host reuse
// connections. http.DefaultTransport keeps only two idle connections per
// host, so most connections of a parallel round were closed and dialed again.
// 64 idle connections cover the parallel requests of the default profile:
// render.maxConcurrency 4 × 8 local Helm blob reads × base and head. Higher
// concurrency still works and only redials beyond 64. Sharing the pool across
// instances is safe: credentials are request headers, and clients have no
// per-instance TLS or proxy settings.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	return t
}()

// userAgentVersion is set by release packaging with a Go linker flag.
var userAgentVersion = "0.1.0-dev"

func New(base, token string, timeout time.Duration, limit int64) *Client {
	return NewWithTokenEnv(base, token, "", timeout, limit)
}

// NewWithTokenEnv is New for a token read from the environment variable
// tokenEnv; access diagnostics name the variable, never its value.
func NewWithTokenEnv(base, token, tokenEnv string, timeout time.Duration, limit int64) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, TokenEnv: tokenEnv, Limit: limit, HTTP: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// backendMessage keeps the message of a gRPC-gateway error body, such as
// Argo CD's {"message": "charts/api: app path does not exist"}, as one line
// of at most 300 characters, for adapters to match known causes. Other bodies
// are not kept.
func backendMessage(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, 4<<10))
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &response) != nil {
		return ""
	}
	message := strings.Join(strings.Fields(response.Message), " ")
	if runes := []rune(message); len(runes) > 300 {
		message = string(runes[:300]) + "…"
	}
	return message
}

func (c *Client) Get(ctx context.Context, p string, query url.Values, out any) error {
	u := c.Base + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	resp, err := c.send(ctx, u, true, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := c.responseFailure(resp); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.Limit+1))
	if err != nil {
		return domain.Fail(domain.CodeResponseInvalid, "decode", "cannot read backend response", "check backend availability", err)
	}
	if int64(len(b)) > c.Limit {
		return domain.LimitError(domain.CodeLimitExceeded, "decode", "API response", "render.maxResponseBytes", int64(len(b)), c.Limit, "bytes", true, domain.MaxRenderResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err = decoder.Decode(out); err != nil {
		return undecodable(resp.Header.Get("Content-Type"), err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return domain.Fail(domain.CodeResponseInvalid, "decode", "backend returned multiple JSON values", "check backend compatibility", nil)
	}
	return nil
}

// mediaTypeName is a media type without parameters, such as "text/html".
var mediaTypeName = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)

// undecodable is the failure of a 200 response that is not JSON. Its media
// type is a safe fact for the diagnostic; an HTML page usually comes from an
// SSO or login gateway in front of the API, which the action names
// .
func undecodable(contentType string, cause error) error {
	reason, action := "backend response is not valid JSON", "check backend compatibility"
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if !mediaTypeName.MatchString(mediaType) {
		mediaType = ""
	}
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		reason = "backend returned an HTML page instead of JSON, as an SSO or login gateway does"
		action = "check that the Argo CD endpoint or git.apiURL names the API itself, such as https://HOST/api/v3 for GitHub Enterprise Server, and that no login proxy answers in front of it"
	}
	diagnostic := domain.Locate(cause).With(func(d *domain.Diagnostic) {
		d.ContentType = mediaType
	})
	return &domain.Failure{Issue: domain.Issue{Code: domain.CodeResponseInvalid, Stage: "decode", Reason: reason, Action: action, Diagnostic: diagnostic}, Cause: cause}
}

// send sends one GET for target, with the client credential when credential
// is set, and returns the response of any status.
func (c *Client) send(ctx context.Context, target string, credential bool, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, domain.Fail(domain.CodeRequestInvalid, "connection", "cannot construct API request", "check the configured endpoint", nil)
	}
	if credential && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "diffaro/"+userAgentVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Keep the cause, such as a deadline or cancellation, without the
		// URL, which may carry a query or a signed download location.
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return nil, transportFailure(c.Base, err)
	}
	return resp, nil
}

// responseFailure maps a non-200 response to a rate_limited, access_denied,
// not_found or backend_error failure, and returns nil for 200.
func (c *Client) responseFailure(resp *http.Response) error {
	if err := RateLimited(resp); err != nil {
		return err
	}
	switch resp.StatusCode {
	case 200:
		return nil
	case 401, 403:
		return c.statusFailure(resp.StatusCode, "", domain.CodeAccessDenied, "permission")
	case 404:
		return c.statusFailure(resp.StatusCode, "", domain.CodeNotFound, "lookup")
	default:
		return c.statusFailure(resp.StatusCode, backendMessage(resp.Body), domain.CodeBackendError, "connection")
	}
}
