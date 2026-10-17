package gateway

import (
	"errors"
	"net"
	"net/url"
)

// requestNeverReachedUpstream reports whether a transport error happened before
// any byte of the request reached the upstream service.
//
// This is the one case where replaying a non-idempotent request is safe: if the
// TCP connection was never established, the upstream cannot have created an
// image, a video or a synthesised audio clip, so trying another provider is not
// a duplicate operation. Anything else — a read timeout, an unexpected EOF, a
// reset after the body was written — may have been accepted, and is left on the
// existing "do not replay" path.
//
// The check is deliberately narrow. Go's dial errors (including DNS resolution
// failures, which are wrapped as OpError{Op:"dial"}) are the only shape treated
// as "never sent"; TLS handshake failures also happen before the HTTP request
// is written, but they are excluded to keep the rule conservative.
func requestNeverReachedUpstream(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	var opErr *net.OpError
	if !errors.As(urlErr.Err, &opErr) {
		return false
	}
	return opErr.Op == "dial" || opErr.Op == "proxyconnect"
}
