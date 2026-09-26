// Package meshclient builds the HTTP clients a node uses to talk to other
// members: capacity and status polls, the activity followers behind
// /v1/mesh/stream, prompt lookups, power forwards and inference forwards.
//
// Every member-traffic client shares three properties, which is why they are
// built in one place rather than declared wherever they are needed:
//
//   - The dial is bounded. A powered-off host drops packets rather than
//     refusing, so an unbounded dial runs to the kernel's connect timeout
//     (minutes) and pins whatever goroutine made the call.
//   - Proxy variables are ignored. One inherited by a container must not route
//     tailnet traffic elsewhere.
//   - The overall timeout is per use, not per package. A poll is bounded by one
//     interval, a prompt lookup by seconds, and a stream or a long generation by
//     nothing at all — for those only the dial and, optionally, the wait for
//     response headers are bounded, and cancellation comes from a context.
package meshclient

import (
	"net"
	"net/http"
	"time"
)

// DefaultDialTimeout bounds the TCP handshake when Options leaves it zero.
const DefaultDialTimeout = 5 * time.Second

// defaultIdlePerHost suits a client that makes one request at a time per
// member, which every poller and follower does.
const defaultIdlePerHost = 2

// Options configures a member-traffic client. The zero value is a client with
// a bounded dial and no overall timeout.
type Options struct {
	// DialTimeout bounds the TCP handshake; 0 = DefaultDialTimeout.
	DialTimeout time.Duration
	// Timeout bounds the whole request including reading the body; 0 = none.
	// Leave it zero for streams and inference, which run for minutes.
	Timeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers after the
	// request is written; 0 = none. Safe for a stream whose server answers
	// headers at once, and wrong for a non-streaming completion, which
	// withholds its headers until the generation ends.
	ResponseHeaderTimeout time.Duration
	// MaxIdleConnsPerHost; 0 = 2.
	MaxIdleConnsPerHost int
}

// New returns a client for member traffic.
func New(o Options) *http.Client {
	dial := o.DialTimeout
	if dial <= 0 {
		dial = DefaultDialTimeout
	}
	idle := o.MaxIdleConnsPerHost
	if idle <= 0 {
		idle = defaultIdlePerHost
	}
	return &http.Client{
		Timeout: o.Timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: dial, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: o.ResponseHeaderTimeout,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   idle,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// Default is the shared client for one-off member requests that bound their
// own duration with a context. Its dial is bounded; nothing else is.
var Default = New(Options{})
