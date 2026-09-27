package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/mesh/meshclient"
	"github.com/janit/viiwork/v2/meshapi"
)

type outcome int

const (
	outcomeServed    outcome = iota // a response (any status) went to the client, possibly cut short
	outcomeRetryable                // nothing was written to the client; another route may serve it
)

type execResult struct {
	Outcome outcome
	Status  int    // status written to the client when served
	Aborted bool   // the client went away mid-response
	Reason  string // why it is retryable, or why it was truncated
	// Truncated: the upstream failed after the response headers went out, so
	// the client holds a partial response. The handler ends it with
	// http.ErrAbortHandler: a response that ends cleanly would look complete.
	Truncated bool
	// Refusal: the retryable failure was a capacity refusal — a 429 or 503,
	// a backend with no process, or no connection at all — rather than a
	// failure of the request itself. Only refusals make an exhausted dispatch
	// a 503 with Retry-After instead of a 502.
	Refusal bool
}

func retryable(format string, args ...any) execResult {
	return execResult{Outcome: outcomeRetryable, Reason: fmt.Sprintf(format, args...)}
}

func refused(format string, args ...any) execResult {
	return execResult{Outcome: outcomeRetryable, Refusal: true, Reason: fmt.Sprintf(format, args...)}
}

// isNoConnection reports whether err means no connection was made at all: the
// dial failed or was refused, so the request never reached the other side.
func isNoConnection(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// isHardSocketFailure reports whether err from a backend request means the
// backend's listener is gone: EOF before a response header (the process closed
// the connection mid-request), or refused/reset from the dialer (the port is no
// longer bound). These are kernel-level signals the inference path acts on at
// once without waiting for /health. Context errors are excluded, so a client
// cancelling does not evict a healthy backend. Copied from v1.
func isHardSocketFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// hopByHopHeaders are HTTP/1.1 headers a proxy must not forward (RFC 7230).
// Copied from v1.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// backendClient has no timeout: inference streams for minutes, and the
// client's request context controls cancellation. Copied from v1.
var backendClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// peerDialTimeout bounds only the handshake to a peer. A powered-off host drops
// packets rather than refusing, so a dial would otherwise run to the kernel's
// connect timeout (v1.8.1).
const peerDialTimeout = 5 * time.Second

// peerClient has no overall timeout (Decision 10): v1's 120 s cap cut off long
// generations, and a non-streaming completion withholds its headers until the
// generation ends. Cancellation comes from the client's context. Mesh traffic
// ignores proxy variables, so one inherited by a container cannot route
// tailnet traffic elsewhere.
var peerClient = meshclient.New(meshclient.Options{DialTimeout: peerDialTimeout, MaxIdleConnsPerHost: 100})

func targetURL(addr string, r *http.Request) string {
	u := "http://" + addr + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

// isCORSHeader reports whether key is Origin or an Access-Control-* header.
//
// CORS belongs to the node alone (internal/node's CORS layer); an engine or a
// peer must never take part. llama-server echoes any Origin it is sent into
// Access-Control-Allow-Origin, so forwarding Origin and copying the answer
// back would let an origin the node refused read inference output, and give
// an allowed one a second allow header, which browsers reject.
func isCORSHeader(key string) bool {
	const prefix = "Access-Control-"
	if len(key) >= len(prefix) && strings.EqualFold(key[:len(prefix)], prefix) {
		return true
	}
	return strings.EqualFold(key, "Origin")
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] || isCORSHeader(key) {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// copyResponseHeaders copies an engine's or a peer's response headers to the
// client, less hop-by-hop headers, every CORS header (see isCORSHeader) and
// Origin in Vary, which the node's CORS layer sets itself. Content-Length is
// dropped too when dropLength is set, for a body that will be rewritten.
func copyResponseHeaders(dst, src http.Header, dropLength bool) {
	for key, values := range src {
		ck := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[ck] || isCORSHeader(ck) || (dropLength && ck == "Content-Length") {
			continue
		}
		if ck == "Vary" {
			values = varyWithoutOrigin(values)
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// varyWithoutOrigin removes Origin from Vary values and keeps every other
// token. Values that cannot mention Origin are returned as they are.
func varyWithoutOrigin(values []string) []string {
	mentions := false
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), "origin") {
			mentions = true
			break
		}
	}
	if !mentions {
		return values
	}
	var out []string
	for _, v := range values {
		var kept []string
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" && !strings.EqualFold(tok, "Origin") {
				kept = append(kept, tok)
			}
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, ", "))
		}
	}
	return out
}

// serveLocal runs the request on a local backend. Nothing is written to the
// client unless a response the client should see arrived: a transport failure
// or a 429/503 from the engine is retryable, and a hard socket failure also
// tells the backend's supervision loop to probe at once.
func serveLocal(w http.ResponseWriter, r *http.Request, body []byte, b route.LocalBackend, model, self string, thinkDisabled bool) execResult {
	addr := b.Addr()
	if addr == "" {
		return refused("backend %s has no running process", b.ID())
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL(addr, r), bytes.NewReader(body))
	if err != nil {
		return retryable("backend %s: %v", b.ID(), err)
	}
	copyRequestHeaders(req.Header, r.Header)
	req.ContentLength = int64(len(body))

	resp, err := backendClient.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return execResult{Outcome: outcomeServed, Aborted: true}
		}
		if isHardSocketFailure(err) {
			b.NoteHardFailure()
		}
		if isNoConnection(err) {
			return refused("backend %s: %v", b.ID(), err)
		}
		return retryable("backend %s: %v", b.ID(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		return refused("backend %s answered %d", b.ID(), resp.StatusCode)
	}

	sse := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	var rewritten []byte
	if thinkDisabled && !sse {
		// Read before touching the client's headers: a body cut off here is
		// retryable, and the next attempt must not inherit this one's headers.
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return retryable("backend %s: reading the response: %v", b.ID(), err)
		}
		rewritten = rewriteThinkResponse(raw)
	}

	copyResponseHeaders(w.Header(), resp.Header, thinkDisabled)
	w.Header().Set(meshapi.HeaderNode, self)
	w.Header().Set(meshapi.HeaderGPUBackend, b.ID())
	w.Header().Set(meshapi.HeaderModel, model)

	res := execResult{Outcome: outcomeServed, Status: resp.StatusCode}
	if thinkDisabled && !sse {
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
		w.WriteHeader(resp.StatusCode)
		_, err := w.Write(rewritten)
		return endStream(res, r, err != nil, nil, "backend "+b.ID())
	}
	w.WriteHeader(resp.StatusCode)
	var clientGone bool
	var upstreamErr error
	if thinkDisabled {
		clientGone, upstreamErr = streamThinkDisabled(w, resp.Body, cancel)
	} else {
		clientGone, upstreamErr = stream(w, resp.Body, cancel)
	}
	return endStream(res, r, clientGone, upstreamErr, "backend "+b.ID())
}

// endStream completes the result of a response whose headers went out. A
// client that went away is Aborted; an upstream that failed while the client
// was still there is Truncated.
func endStream(res execResult, r *http.Request, clientGone bool, upstreamErr error, who string) execResult {
	res.Aborted = clientGone || r.Context().Err() != nil
	if upstreamErr != nil && !res.Aborted {
		res.Truncated = true
		res.Reason = fmt.Sprintf("%s: response cut short: %v", who, upstreamErr)
	}
	return res
}

// forwardToPeer runs the request on a peer. The body is passed explicitly
// because the forward signature covers its digest. The peer's response goes to
// the client byte for byte: thinking is rewritten once, on the executing node
// (Decision 9).
func forwardToPeer(w http.ResponseWriter, r *http.Request, body []byte, t route.Target, auth *ForwardAuth, self string) execResult {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL(t.Addr, r), bytes.NewReader(body))
	if err != nil {
		return retryable("peer %s: %v", t.Node, err)
	}
	copyRequestHeaders(req.Header, r.Header)
	req.ContentLength = int64(len(body))
	if err := auth.Sign(req, body); err != nil { // last: the signature covers the request
		return retryable("peer %s: signing the forward: %v", t.Node, err)
	}

	resp, err := peerClient.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return execResult{Outcome: outcomeServed, Aborted: true}
		}
		if isNoConnection(err) {
			return refused("peer %s: %v", t.Node, err)
		}
		return retryable("peer %s: %v", t.Node, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		return refused("peer %s answered %d", t.Node, resp.StatusCode)
	}

	copyResponseHeaders(w.Header(), resp.Header, false)
	w.Header().Set(meshapi.HeaderOrigin, self)
	if w.Header().Get(meshapi.HeaderNode) == "" {
		w.Header().Set(meshapi.HeaderNode, t.Node)
	}
	w.WriteHeader(resp.StatusCode)
	// A failure after this point ends the response as it stands: the peer may
	// already be generating, so it is never retried. A peer that cut its
	// response short is Truncated, and the client's response is aborted in turn.
	clientGone, upstreamErr := stream(w, resp.Body, func() {})
	return endStream(execResult{Outcome: outcomeServed, Status: resp.StatusCode}, r, clientGone, upstreamErr, "peer "+t.Node)
}

// stream copies body to w, flushing after each read. A client write error
// cancels the upstream request and reports the client gone. An upstream read
// error other than io.EOF is returned, so a cut response is never mistaken for
// a complete one.
func stream(w http.ResponseWriter, body io.Reader, cancel func()) (clientAborted bool, upstreamErr error) {
	f, ok := w.(http.Flusher)
	if !ok {
		// io.Copy cannot say which side failed. A client that went away also
		// cancels the request context, which endStream checks first.
		_, err := io.Copy(w, body)
		return false, err
	}
	buf := make([]byte, 4096)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				cancel()
				return true, nil
			}
			f.Flush()
		}
		if readErr == io.EOF {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}
