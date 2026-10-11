package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// finalReason is the reason a transport error gives, and nothing of where it
// happened: "no such host", "connection refused", "i/o timeout", the TLS
// reason. net's errors name what was dialed or looked up ("dial tcp
// 127.0.0.1:1: connect: connection refused", "lookup <name>: no such host"),
// which is exactly what RedactURL withholds, so a URL whose host is
// withheld reports this instead of the cause as Go words it. A cause it
// does not know gets a fixed text: its words could hold the address.
func finalReason(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return "no such host"
		case dns.IsTimeout:
			return "i/o timeout"
		}
		return "name lookup failed"
	}
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownCA):
		return "x509: certificate signed by unknown authority"
	case errors.As(err, &hostname):
		return "x509: certificate is not valid for the host"
	case errors.As(err, &invalid):
		if invalid.Reason == x509.Expired {
			return "x509: certificate has expired or is not yet valid"
		}
		return "x509: certificate is not valid"
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return "x509: certificate verification failed"
	}
	var errno syscall.Errno
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &errno):
		return errno.Error() // "network is unreachable", "no route to host", ...
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed (EOF)"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "i/o timeout"
	}
	// An *net.OpError is "<op> <net> <src>-><addr>: <cause>": the cause is
	// worth a second look, the rest is the address.
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil && op.Err != err {
		if strings.HasPrefix(op.Err.Error(), "tls: ") || strings.HasPrefix(op.Err.Error(), "remote error: tls: ") {
			return op.Err.Error()
		}
		return finalReason(op.Err)
	}
	if r := checkReason(err); r != "does not parse" {
		return strings.TrimPrefix(r, "does not parse: ") // a URL net/http could not build a request from
	}
	if strings.HasPrefix(err.Error(), "tls: ") && !strings.ContainsAny(err.Error(), "0123456789") {
		return err.Error() // "tls: handshake failure" and the like
	}
	return "request failed"
}

// checkReason is the fixed text for the way url.Parse rejected a URL. It
// reads net/url's error to classify it and never repeats it: that error
// quotes part of the value ("invalid port \":SECRET\" after host").
func checkReason(err error) string {
	var esc url.EscapeError
	var host url.InvalidHostError
	msg := err.Error()
	switch {
	case errors.As(err, &esc):
		return "does not parse: a bad percent-escape"
	case errors.As(err, &host):
		return "does not parse: a bad host"
	case strings.Contains(msg, "invalid port"):
		return "does not parse: the port is not a number"
	case strings.Contains(msg, "control character"):
		return "does not parse: a control character"
	case strings.Contains(msg, "invalid userinfo"):
		return "does not parse: a bad user part"
	case strings.Contains(msg, "missing protocol scheme"), strings.Contains(msg, "first path segment"), strings.Contains(msg, "invalid URI for request"):
		return "does not parse: no scheme"
	case strings.Contains(msg, "in host name"), strings.Contains(msg, "invalid host"), strings.Contains(msg, "IP-literal"):
		return "does not parse: a bad host"
	}
	return "does not parse"
}
