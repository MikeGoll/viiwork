package node

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// selfHost is the placeholder host of URLs this node sends to its own API.
// selfTransport replaces it with the listener's real address; it is never
// resolved.
const selfHost = "viiwork.self"

// pipelineStepTimeout bounds one pipeline step's call, as pipeline.Executor's
// own default client did.
const pipelineStepTimeout = 120 * time.Second

// selfDialAddr is the address this node dials to reach its own API, given the
// address the listener bound. A wildcard bind is reached over loopback; a
// specific address — api.host set to a tailnet or LAN IP — only at that
// address, which is why "127.0.0.1:<port>" is not good enough.
func selfDialAddr(a net.Addr) string {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return a.String()
	}
	ip := tcp.IP
	switch {
	case ip == nil || ip.Equal(net.IPv4zero):
		ip = net.IPv4(127, 0, 0, 1)
	case ip.IsUnspecified():
		ip = net.IPv6loopback
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port))
}

// wildcardHost reports whether an api.host value binds every interface.
func wildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// selfTransport sends a request to this node's own API listener, at the
// address it actually bound.
type selfTransport struct {
	addr func() string
	base http.RoundTripper
}

func (t selfTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	addr := t.addr()
	if addr == "" {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("node: the API is not listening yet")
	}
	r := req.Clone(req.Context())
	r.URL.Host = addr
	r.Host = ""
	return t.base.RoundTrip(r)
}

// selfClient is the HTTP client for calls into this node's own API (pipeline
// steps). No proxy: the environment's HTTP_PROXY must never see a call to
// ourselves, and with api.host set to a routable address it otherwise would.
func (n *Node) selfClient() *http.Client {
	return &http.Client{
		Timeout: pipelineStepTimeout,
		Transport: selfTransport{
			addr: func() string { return n.selfAddr.Load().(string) },
			base: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}
