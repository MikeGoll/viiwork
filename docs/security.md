# Security and the Trust Model

**viiwork authenticates nothing.** Every API endpoint and every dashboard is open
to any client that can reach the server. Reachability *is* the authorization
model, and the fleet is expected to sit on a tailnet or another trusted network.
If you expose viiwork to an untrusted network, put a reverse proxy (Caddy, nginx)
or firewall rules in front of it.

- [What that exposes](#what-that-exposes)
- [Browser origins (CORS)](#browser-origins-cors)

## What that exposes

- **Prompt *and response* text is readable over the API.** The history
  (`activity.prompt_history` requests per node, 1000 by default, in memory) is
  served unauthenticated like everything else. If either side of the traffic on
  your fleet is sensitive, restrict access at the network layer. There is
  currently no switch to disable the history. See
  [dashboards.md](dashboards.md#prompt-and-output-history).
- **Aliases change what every client gets.** In a secured mesh a write needs the
  mesh secret's signature; in an open mesh it is accepted only from the node's own
  machine. A reverse proxy on that machine in front of the API would make every
  request look local, so put one only in front of a secured mesh. See
  [mesh.md](mesh.md#aliases).
- **Membership is the mesh secret's job.** Without a secret, any viiwork node that
  reaches 7946 joins — which on a tailnet means the tailnet's ACLs are the
  control. See [mesh.md](mesh.md#secured-and-open).
- **Chassis power control is off by default and has no wildcard.** The allowlist
  is the whole authorization story; anyone who can reach the API can use what it
  names. See [power-and-energy.md](power-and-energy.md#power-control).

Two endpoints are deliberately narrowed, because an unauthenticated API on a
network that also carries IPMI would otherwise be a probe primitive:

- **`/v1/mesh/prompt` only forwards to mesh members.** The `addr` parameter is
  checked against the API addresses of alive members before anything is fetched.
- **`?host=` is compared, never dialled.** The value only filters routing
  candidates by node name; it never becomes an address.

Mesh headers are stripped from every incoming request right after verification,
so no client can smuggle a forwarding claim through and no engine ever sees one.

## Browser origins (CORS)

Server-side callers — curl, a backend proxying on behalf of its own UI — are
unaffected by any of this. It matters only when a page served from somewhere else
fetches viiwork directly from the browser.

Because viiwork authenticates nothing, an origin allowlist is not protecting the
API from anyone who can already reach it. What it stops is a page in some browser
on your network quietly driving your fleet through that browser's network
position. Treat the list as a real control and keep it short:

```yaml
api:
  cors:
    allow_origins: ["*.ts.net", "localhost", "127.0.0.1", "*.your-app.example"]
    allow_tailnet_ips: true   # also 100.64.0.0/10 and fd7a:115c:a1e0::/48
```

`*.example.com` matches subdomains only, never the bare apex; every other entry
must match the host exactly. `allow_origins: []` sends no CORS header at all,
which is how viiwork behaved before v1.1.0.

What ships is `*.ts.net`, `localhost` and `127.0.0.1` plus tailnet IPs — the
deployment viiwork documents, and nothing else. Your own application's origin is
deployment-specific: add it in your `viiwork.yaml`, never in the repo's defaults.

Two behaviours are pinned by tests: **SSE streams carry the header on their own
GET response** (`EventSource` sends no preflight), and **`OPTIONS` is answered
before routing**, with a refused preflight returning 403.

Where the consumer has a backend of its own, **prefer a server-side proxy over
CORS**: it needs no allowlist entry, and it can put authentication in front of an
API that has none. [api-integration.md](api-integration.md) covers both modes.
