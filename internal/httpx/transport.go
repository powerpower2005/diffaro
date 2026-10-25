package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"

	"diffaro/internal/domain"
)

// transportFailure is the connection_failed failure of a request that got
// no response. It names the host, never the URL, which may carry a query or
// a signed location, and states the transport cause with the action for it. The cause types are the ones the debug runbook reads from
// diagnostic.chain.
func transportFailure(base string, err error) error {
	host := "the server"
	if u, parseErr := url.Parse(base); parseErr == nil && u.Host != "" {
		host = u.Host
	}
	reasonCode, problem, action := transportCause(err)
	return domain.FailReason(domain.CodeConnectionFailed, reasonCode, "connection", "API request to "+host+" "+problem, action, err)
}

// TransportCause classifies the transport error of a request that got no
// response for an adapter with its own client: the reason code and the
// problem phrase, such as "failed: the host name could not be resolved".
// The reason code is "" when no cause is recognized.
func TransportCause(err error) (reasonCode, problem string) {
	reasonCode, problem, _ = transportCause(err)
	return reasonCode, problem
}

func transportCause(err error) (reasonCode, problem, action string) {
	var dns *net.DNSError
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var timeout net.Error
	switch {
	case errors.As(err, &dns):
		return domain.ReasonDNSFailed, "failed: the host name could not be resolved", "check the host name of the Argo CD endpoint or git.apiURL, and the DNS, VPN or proxy of this machine"
	case errors.As(err, &verification), errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid):
		return domain.ReasonTLSUntrusted, "failed: the server's TLS certificate was not accepted", "add the CA that signed the server certificate to this machine's trust store, or check that the endpoint host matches the certificate"
	case isAny(err, refusedErrnos):
		return domain.ReasonConnectionRefused, "failed: the connection was refused", "check the endpoint port, and that the server or port-forward is running and reachable from this machine"
	case isAny(err, resetErrnos), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return domain.ReasonConnectionClosed, "failed: the server or a proxy closed the connection", "check the proxy or VPN between this machine and the server, then retry"
	case errors.As(err, &timeout) && timeout.Timeout():
		return domain.ReasonTimeout, "timed out", "check that the server is reachable from this machine; for a slow server raise render.timeout"
	}
	return "", "failed", "check connectivity, TLS and timeout settings"
}

func isAny(err error, targets []error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
