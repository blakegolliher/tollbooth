# ADR-0008: Backends through a resolver outage: remember the addresses that resolved, and use them when the lookup fails

Status: **accepted** (Blake, 2026-11-10, answering Gate 5 question 8: "fix before the tag", and the
scope question that followed: "health check and new connections"). It changes how the backend pool
opens connections and what the health check concludes from a failed name lookup, which is why it is
an ADR and not a patch.

## Context

On 2026-11-10 at 18:29Z, with tollbooth in front of a VAST lab cluster named by DNS
(`backend.endpoints: [main.selab-var204…:80]`), the dev box's DNS server stopped answering. What
the proxy did, from its own log and admin API:

| Time | What happened |
| --- | --- |
| 18:29:49Z | The health probe (`HEAD /` every 5 s on a fresh connection) hung in the name lookup and hit its 5 s timeout. The only endpoint was marked **down**; `/readyz` went **not ready**. |
| 18:30 onward | Every probe failed: `lookup …: i/o timeout`, and after the box was pointed at a public resolver, `no such host`. The endpoint stayed down for the rest of the day. |
| the whole time | The proxy served 100 req/s with **0 errors**, 225,736 requests by 19:07Z, on the keep-alive connections it already had. The cluster answered `HEAD /` on its addresses directly. |

Two things are wrong in that picture, and they have the same cause.

1. **Readiness lied.** A load balancer acting on `/readyz` would have pulled a node that was
   serving every request. With several nodes behind one resolver, it would have pulled all of them.
2. **The serving was luck.** Requests succeeded only because they reused open connections. A
   request that needed a new one would have failed in the same lookup and been answered 503.

The cause: the pool treats a backend's name as its address. Both the health probe's transport and
the data path's transport hand `host:port` to `net.Dialer`, which resolves the name on every new
connection. Nothing in the proxy remembers what the name resolved to a second ago. The
specification's priority order puts availability second, after transparency; a resolver is a
dependency the data path does not need to have while the backend's addresses have not changed.

What the code does today (`internal/proxy/backend.go`): any error from the probe marks the endpoint
down, any HTTP status marks it up; on the data path a connect error marks it down and the request
moves to another endpoint if no body byte was sent; with every endpoint down `Next` still returns
one, so requests are attempted (fail open). Endpoints given as IP literals never touch the resolver
and were never affected.

## Decision

**The pool remembers, per endpoint, the addresses its name last resolved to, and uses them when a
lookup fails.** One dial function serves both the data path and the health probe:

1. An endpoint written as an IP literal is dialed as it is. Nothing changes for it.
2. For a named endpoint the pool looks the name up itself, with half of the dial timeout as the
   lookup's budget (`backend.dial_timeout` on the data path, `backend.health_check.timeout` for the
   probe), so that a resolver that hangs leaves time to connect.
3. **The lookup succeeds:** the addresses are remembered, replacing the previous ones, and dialed in
   the resolver's order, the first that connects wins. DNS is still asked on every new connection,
   so a cluster that rotates its answers (VAST's VIP pools do) spreads connections as before, and a
   re-addressed cluster is picked up on the next connection.
4. **The lookup fails and addresses are remembered:** they are dialed instead, starting at a
   rotating position. The failure is counted (`tollbooth_backend_errors_total{kind="resolve"}`),
   shown (`backends list`, the Backends screen, `GET /admin/backends`), and logged once when it
   starts and once when the resolver answers again. It does **not** mark the endpoint down.
5. **The lookup fails and nothing is remembered** (the proxy started during the outage): the dial
   fails as a connect error of kind `resolve`. The endpoint is marked down and the request moves to
   another endpoint, as for any connect error today. There is nothing better to do: the proxy has
   never known where that backend is.

Health keeps its rule: the probe marks an endpoint up on any HTTP status and down on any error.
What changes is that a failed lookup is no longer an error of the probe when an address is known:
the probe reaches the backend at the remembered address and reports what it finds there. If the
backend is really gone, the probe fails to connect and the endpoint goes down, as it should.

No configuration key is added. Remembered addresses live for the life of the process and are
replaced by every successful lookup; there is no expiry, because an expiry would bring back the
failure this ADR removes, only later.

## Alternatives considered

| Option | What it does in a resolver outage | Why not |
| --- | --- | --- |
| **A. Health check only**: a failed lookup does not mark the endpoint down; the probe uses the last address | Readiness stays "ready"; requests that need a new connection still fail with 503 | It makes `/readyz` say ready about a node that is failing a growing share of requests as its connections turn over. Fixes the lie in one direction by telling it in the other. Blake chose the wider scope |
| **B. This ADR** | Readiness reflects the backend at its last known address; new connections use that address | — |
| C. Document "list endpoints by address" and change nothing | Works for operators who follow it | It is the workaround already in the runbook; it gives up DNS-based VIP rotation, which is how a VAST cluster spreads clients |
| D. A caching resolver with a TTL inside the proxy | Serves stale answers until the TTL runs out | Go's resolver does not expose record TTLs; any TTL chosen here is a guess, and when it expires during an outage the failure returns |
| E. Leave it to a local caching resolver on the host (`systemd-resolved`, `nscd`, `dnsmasq`) | Depends on that resolver's stale-serving behavior | A host dependency this project cannot verify; `doctor` has nothing to check it with |

## Consequences

- **Connection establishment for named endpoints is now this project's code**, about 60 lines:
  one lookup, then the addresses tried one after another with the remaining time divided among
  them. Go's dialer races IPv6 against IPv4 when a name has both (RFC 6555); this does not. An S3
  backend is reached over one address family in every deployment this project knows; a dual-stack
  backend whose first family is unreachable costs one failed attempt per new connection.
- Bytes on the wire are unchanged: the `Host` header, the TLS server name, and certificate
  verification still use the configured name (only the dial target is an address).
- If a cluster is re-addressed **during** a resolver outage, the remembered addresses are wrong:
  dials fail, the endpoint is marked down, and the proxy answers 503 until the resolver returns.
  That is the behavior before this ADR, reached later and only in that case.
- The `kind` label of `tollbooth_backend_errors_total` gains the value `resolve`
  (`docs/telemetry-catalog.md`); `GET /admin/backends` gains `errors_resolve`, `addresses`, and
  `resolver_failing_since`; `backends list` gains a `RESOLVE` column.
- `docs/OPERATIONS.md` §10's resolver row is rewritten: the workaround it gave is no longer needed.
- The proxy still starts "optimistic": an endpoint is up until its first probe. A start during a
  resolver outage therefore reports ready for at most one probe interval and then not ready, which
  is correct: it cannot reach a backend it has never located.

## Tests that pin it

`internal/proxy`: a named endpoint served from a local listener through an injected lookup. The
lookup then fails; a new connection is forced; the request succeeds, the endpoint stays up, the
probe passes, `resolve` is counted, and the status row says since when. The same with a lookup
that hangs until its deadline. A start with a failing lookup: connect error, endpoint down, not
ready. A remembered address that no longer accepts connections while the lookup fails: endpoint
down. A new answer from the resolver replaces the remembered addresses. An IP-literal endpoint
never calls the lookup.
