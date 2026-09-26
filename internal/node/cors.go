package node

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/janit/viiwork/v2/meshapi"
)

// CORS lets browser code on another origin read this node's API.
//
// viiwork has no authentication: reachability is the authorization model, and
// the fleet is expected to sit on a tailnet. That is what makes an origin
// allowlist meaningful here rather than theatre — it is not protecting the API
// from a caller who can already reach it with curl, it is stopping a random
// page in a tailnet member's browser from quietly driving the fleet through
// that member's network position. Keep the list to origins that are themselves
// only reachable, or only served, where you would accept that.
//
// Two endpoint families need this and are easy to forget:
//
//   - The SSE streams (/v1/mesh/stream and friends). EventSource is CORS-bound
//     like any other fetch, but sends no preflight, so it needs the header on
//     the GET response itself.
//   - OPTIONS. Before this existed the router matched only GET and POST, so
//     every preflight 404'd and no cross-origin POST could work at all.
type CORS struct {
	// Origins are host patterns, not URLs. A leading "*." matches any
	// subdomain ("*.example.com" matches app.example.com but not example.com
	// itself); anything else must match the host exactly.
	Origins []string
	// TailnetIPs additionally allows origins addressed by a literal Tailscale
	// IP. MagicDNS names are the common case, but a tailnet is just as often
	// browsed by its 100.x address, and an allowlist that quietly failed for
	// half the ways you reach the same host would be worse than no allowlist.
	TailnetIPs bool
}

// Tailscale hands out IPv4 from the CGNAT block 100.64.0.0/10 and IPv6 from
// fd7a:115c:a1e0::/48.
var (
	tailnetV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	tailnetV6 = mustCIDR("fd7a:115c:a1e0::/48")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("node: bad built-in CIDR " + s + ": " + err.Error())
	}
	return n
}

// exposedHeaders are the viiwork-specific response headers a browser client is
// allowed to read. Without this a cross-origin fetch can see the body but not
// which backend served it, which is most of what those headers are for. v2
// adds the node, model, alias and queue-time headers of contract C5.
const exposedHeaders = "X-GPU-Backend, X-Queue-Depth, X-Viiwork-Origin, X-Pipeline, X-Pipeline-Locale, X-Pipeline-Steps, X-Viiwork-Node, X-Viiwork-Model, X-Viiwork-Alias, X-Viiwork-Queued-Ms"

// Allows reports whether origin (an Origin header value) may read this API.
func (c *CORS) Allows(origin string) bool {
	if c == nil || origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Only the two schemes a browser sends for a page fetch. Anything else is
	// an extension origin or a non-browser caller spelling Origin by hand,
	// neither of which should widen the surface.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if c.TailnetIPs {
		if ip := net.ParseIP(host); ip != nil {
			if tailnetV4.Contains(ip) || tailnetV6.Contains(ip) {
				return true
			}
		}
	}
	for _, pat := range c.Origins {
		if strings.HasPrefix(pat, "*.") {
			// Subdomains only. Letting a "*." rule also match the bare apex
			// would silently widen "*.ts.net" into "ts.net", which is somebody
			// else's domain.
			suffix := pat[1:]
			if len(host) > len(suffix) && strings.HasSuffix(strings.ToLower(host), suffix) {
				return true
			}
			continue
		}
		if strings.EqualFold(host, pat) {
			return true
		}
	}
	return false
}

// apply writes the CORS response headers for a request and reports whether it
// was a preflight that has now been fully answered.
//
// Vary: Origin is set whether or not the origin is allowed. The response
// genuinely differs by origin, and a shared cache that missed that would hand
// one origin's allow header to another.
func (c *CORS) apply(w http.ResponseWriter, r *http.Request) (handled bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	w.Header().Add("Vary", "Origin")
	allowed := c.Allows(origin)
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", exposedHeaders)
	}

	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	// From here on this is a preflight, which never reaches a route handler.
	if !allowed {
		// A bare 204 with no allow header would fail in the browser too, but
		// identically to a dozen other mistakes. A 403 says which one it was,
		// which matters when the fix is a one-line config change on a host you
		// are not currently looking at.
		http.Error(w, `{"error":{"message":"origin not allowed","type":"forbidden"}}`, http.StatusForbidden)
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		w.Header().Set("Access-Control-Allow-Headers", req)
	} else {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// Cross-site request forgery.
//
// CORS decides which origins may READ a response. It does not stop a browser
// from SENDING a request: a "simple request" (GET, HEAD or POST with a
// text/plain, form or multipart body, or no body at all) goes out with no
// preflight, and the server acts on it whether or not the page may read the
// answer. On an API that authenticates nothing, any web page a tailnet member
// visits could otherwise POST {"host":"gb2","action":"off"} to /v1/mesh/power
// through that member's browser. Two guards close that, both applied before
// routing:
//
//   - A state-changing request (POST, PUT, PATCH, DELETE) carrying an Origin
//     the allowlist refuses is answered 403 — on every endpoint, inference
//     included, since a refused origin must not spend GPU time either. The
//     node's own pages are same-origin and pass; so does every non-browser
//     client, because only browsers send Origin (curl, SDKs, the alias CLI,
//     member forwards and the pipeline executor send none).
//   - The control endpoints (power, alias writes) accept only
//     Content-Type: application/json, which a browser cannot send cross-origin
//     without a preflight — and a refused preflight is 403. This holds even
//     for a browser old enough to omit Origin.
//
// What remains is DNS rebinding: a page that rebinds its own name to a node's
// address is same-origin by every check a server can make. The defence there
// is the same as for everything else on this API: keep the node off networks
// whose browsers you do not trust (api.host).

// stateChanging reports whether a request method can change server state.
func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sameOrigin reports whether origin names the very host the request was sent
// to, as the node's own dashboards do. Compared as host:port, as a browser
// writes both.
func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// refusesCrossSite reports whether a request is a state change from a browser
// origin that is neither this node's own nor on the allowlist. A nil CORS
// allows no foreign origin, so only same-origin pages may write.
func (c *CORS) refusesCrossSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || !stateChanging(r.Method) {
		return false
	}
	return !sameOrigin(origin, r) && !c.Allows(origin)
}

// controlPath reports whether path is a control endpoint: one whose writes
// must be JSON so that no browser can send them as a simple request.
func controlPath(path string) bool {
	return path == meshapi.PathPower || path == meshapi.PathMeshPower ||
		path == meshapi.PathAliases || strings.HasPrefix(path, meshapi.PathAliases+"/")
}

// isJSON reports whether a Content-Type header names application/json,
// parameters (charset) allowed.
func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// guardWrite answers a state-changing request the CSRF rules refuse, and
// reports whether it did. It runs after CORS.apply, so a refusal still
// carries Vary: Origin.
func (c *CORS) guardWrite(w http.ResponseWriter, r *http.Request) (handled bool) {
	if !stateChanging(r.Method) {
		return false
	}
	if c.refusesCrossSite(r) {
		writeGuardError(w, http.StatusForbidden, "forbidden", "origin not allowed")
		return true
	}
	// A bodiless write with no Content-Type is let through: an older alias CLI
	// sends delete and revert that way, and a browser cannot use one to cross
	// sites, since it always sends Origin on a cross-origin write.
	ct := r.Header.Get("Content-Type")
	if ct == "" && r.ContentLength == 0 {
		return false
	}
	if controlPath(r.URL.Path) && !isJSON(ct) {
		writeGuardError(w, http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be application/json")
		return true
	}
	return false
}

func writeGuardError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":%q}}`+"\n", msg, typ)
}
