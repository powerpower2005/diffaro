package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"diffaro/internal/domain"
)

// StatusError preserves the HTTP status without retaining a response body,
// URL, query, headers or credentials. Message is the bounded one-line message
// of a gRPC-gateway error body on a server error. It is never written to a
// report: adapters may only match it against known causes, because messages
// such as Helm template errors can contain values.
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

// RequestDiagnostic is supplied by adapters. Route must be a constant template,
// never a URL or a path containing repository-controlled values.
type RequestDiagnostic struct {
	Service, Instance, Operation, Route, Permission, AuthenticationAction string
	RequiredAccess                                                        *domain.AccessRequirement
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func (c *Client) credentialDescription() (string, string) {
	if !environmentName.MatchString(c.TokenEnv) {
		if strings.TrimSpace(c.Token) == "" {
			return "configured token: not set or empty", "provide a token in the profile or an explicit credential flag; alternatively configure tokenEnv"
		}
		return "configured token: provided", "verify the profile token or explicit credential flag contains the intended token"
	}
	name := c.TokenEnv
	if strings.TrimSpace(c.Token) == "" {
		return name + ": not set or empty", "set " + name + " in the process running Diffaro; .env files are not loaded automatically"
	}
	return name + ": provided", "verify " + name + " contains the intended token"
}

func (c *Client) statusFailure(status int, message, code, stage string) error {
	credential, action := c.credentialDescription()
	reason := (&StatusError{StatusCode: status}).Error()
	switch status {
	case http.StatusUnauthorized:
		reason += "; authentication was rejected (" + credential + ")"
		if strings.TrimSpace(c.Token) != "" {
			action += "; check expiration, revocation and the configured API endpoint; check whether a gateway requires authentication or strips the Authorization header"
		}
	case http.StatusForbidden:
		reason += "; request was forbidden (" + credential + ")"
		action += "; check read-only access and gateway/proxy access policy"
	case http.StatusNotFound:
		reason += "; requested object or revision is unavailable"
		action = "check the identifier, revision availability and read permission; private objects may be hidden from this token"
	default:
		if status >= 300 && status < 400 {
			// Diffaro never follows redirects; one usually means the URL
			// names a page, such as a login gateway, rather than the API
			//.
			reason += "; the server redirected the request, and Diffaro does not follow redirects"
			action = "check that the Argo CD endpoint or git.apiURL names the API itself, such as https://HOST/api/v3 for GitHub Enterprise Server, and not a page or login gateway that redirects"
			break
		}
		reason += "; backend returned a non-success response"
		action = "check backend health, gateway routing and supported API version"
	}
	cause := &StatusError{StatusCode: status, Message: message}
	diagnostic := domain.Locate(cause).With(func(d *domain.Diagnostic) { d.HTTPStatus = status })
	failure := &domain.Failure{Issue: domain.Issue{Code: code, Stage: stage, Reason: reason, Action: action, Diagnostic: diagnostic}, Cause: cause}
	if code == domain.CodeBackendError {
		// A server error may succeed on retry; another status is unexpected.
		failure.Issue.ReasonCode = domain.ReasonUnexpectedStatus
		switch {
		case status >= 500:
			failure.Issue.ReasonCode = domain.ReasonServerError
		case status >= 300 && status < 400:
			failure.Issue.ReasonCode = domain.ReasonRedirected
		}
	}
	if code == domain.CodeAccessDenied {
		switch {
		case strings.TrimSpace(c.Token) == "":
			failure.Issue.ReasonCode = domain.ReasonCredentialMissing
		case status == http.StatusUnauthorized:
			failure.Issue.ReasonCode = domain.ReasonAuthenticationRejected
		case status == http.StatusForbidden:
			failure.Issue.ReasonCode = domain.ReasonRequestForbidden
		}
	}
	return failure
}

// GetFor adds deterministic adapter context to safe transport diagnostics.
// It copies failures so shared causes and independent branches are not mutated.
func (c *Client) GetFor(ctx context.Context, path string, query url.Values, out any, request RequestDiagnostic) error {
	end := domain.TraceHTTP(ctx, request.Service, request.Operation, request.Route)
	err := c.getFor(ctx, path, query, out, request)
	end(ResponseStatus(err), err)
	return err
}

// ResponseStatus is the HTTP status behind a request result: 200 for
// success, the failing status when a response arrived, and 0 otherwise.
func ResponseStatus(err error) int {
	var status *StatusError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &status):
		return status.StatusCode
	}
	return 0
}

func (c *Client) getFor(ctx context.Context, path string, query url.Values, out any, request RequestDiagnostic) error {
	return c.describe(c.Get(ctx, path, query, out), request)
}

// describe adds the request context and access diagnostics of GetFor to a
// failure.
func (c *Client) describe(err error, request RequestDiagnostic) error {
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		return err
	}
	copy := *failure
	if copy.Issue.Code == domain.CodeAccessDenied && request.RequiredAccess != nil {
		requirement := *request.RequiredAccess
		requirement.Instance = request.Instance
		// Ignore any supplied credential label; it must describe this client.
		requirement.CredentialEnv = ""
		if environmentName.MatchString(c.TokenEnv) {
			requirement.CredentialEnv = c.TokenEnv
		}
		copy.Issue.RequiredAccess = &requirement
	}
	label := request.Service
	if request.Instance != "" {
		label += " instance " + request.Instance
	}
	copy.Issue.Reason = fmt.Sprintf("%s %s (GET %s): %s", label, request.Operation, request.Route, copy.Issue.Reason)
	copy.Issue.Diagnostic = copy.Issue.Diagnostic.With(func(d *domain.Diagnostic) { d.Route = request.Route })
	if copy.Issue.Code == domain.CodeLimitExceeded {
		copy.Issue.Action += "; for init use --max-response-bytes; for doctor/diff edit render.maxResponseBytes in the profile"
	}
	if copy.Issue.Code == domain.CodeAccessDenied && strings.TrimSpace(c.Token) != "" {
		var status *StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusForbidden && request.Permission != "" {
			copy.Issue.Action += "; required read access: " + request.Permission
		}
		if request.AuthenticationAction != "" {
			copy.Issue.Action += "; " + request.AuthenticationAction
		}
	}
	return &copy
}
