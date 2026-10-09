# MCP for the private sync server

Research date: 2026-10-03. Repository inspected at `1fe8ac4404d5d39c13d65df48628abcb6d6d385a`. This is research and a proposed integration, not implemented or deployed behavior. User-confirmed initial scope: read family history and summaries from local desktop clients over Tailscale. The capacity target is the current Unraid node.

## Recommendation

The subsequent [design proposal](../superpowers/specs/2026-10-03-single-node-capacity-mcp-design.md) selects operator provisioning and concrete initial limits. Pairing UI and the alternative limits discussed below are research options, not additional implementation requirements.

Add an optional `/mcp` Streamable HTTP adapter to the Go process, with a small read-only tool catalogue, an independently revocable connection credential, and bounded queries through the existing authorization and tenant boundaries. Prefer stateless handling and ordinary JSON responses for these short requests. Avoid creating an MCP session, snapshot materialization, or background subscription for each desktop client. Preserve compatibility with the actual selected desktop clients; do not assume that they already speak the newest protocol.

For this private first version, a deliberately documented pre-provisioned bearer-credential mode is reasonable if the selected clients support supplying an Authorization header. It is not the interoperable MCP OAuth authorization flow. If a required client only supports OAuth, implement that flow or choose an explicitly supported local bridge; do not instruct users to copy their phone's refresh or access credentials.

## Current protocol and SDK: verified upstream facts

The current protocol revision is **2026-07-28**. Unlike 2025-11-25 and earlier, it carries protocol version and client capabilities on each request; `server/discover` is a mandatory server method but an optional client request. Unsupported-version replies identify acceptable versions. [Official versioning documentation](https://modelcontextprotocol.io/docs/2026-07-28/learn/versioning)

Current Streamable HTTP accepts POST at one endpoint and returns JSON or request-scoped SSE. The new revision removes protocol sessions, standalone GET streams, DELETE session termination, and resumable streams. Earlier versions remain separate compatibility behaviors. Origin validation is required; invalid supplied origins receive 403. [2026-07-28 transport specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)

The official Go SDK's latest stable release observed was **v1.8.0**, released September 14, 2026, release commit abbreviated `3f3b699`. Its release notes describe transport resource-exhaustion and session-lifecycle fixes, plus `SupportedProtocolVersions` to narrow the SDK's accepted versions. This is evidence to evaluate v1.8.0, not evidence that it has been integrated or benchmarked here. [Official release](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)

SDK v1.7.0+ supports 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26, and 2024-11-05; the README labels client-side OAuth support experimental. The SDK exposes MCP, OAuth primitives, and protected-resource metadata packages. Its minimum Go version at v1.8.0 is 1.25.0, below this repository's declared Go 1.26.2. Resolve and test transitive dependency changes before pinning it in `Server/go.mod`. [Versioned README](https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.8.0/README.md), [versioned module file](https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.8.0/go.mod), [repository module](../../Server/go.mod)

The SDK requires `StreamableHTTPOptions.Stateless = true` for 2026-07-28. `JSONResponse` selects JSON responses. Legacy protocol compatibility must still be tested with real clients. Some prose in the SDK's stateless section still refers to old unresolved spec discussions; the version-specific requirement and implementation are clearer. [Versioned transport guide](https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.8.0/docs/protocol.md)

Its HTTP options expose request-body limits (default 4 MiB), idle session timeout (zero means never expire), and cancellation propagation for current-protocol requests. Stateless mode does not mint/read session IDs and returns 405 for GET/DELETE. Cross-origin protection is not enabled merely by leaving its deprecated option nil; wrap the handler with explicit origin protection. [Versioned transport implementation](https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.8.0/mcp/streamable.go)

## Authorization and private reachability

MCP authorization is optional at the protocol level; HTTP implementations that support it should follow the specification. The interoperable path uses an OAuth 2.1 authorization server and an MCP resource server. Each HTTP request carries a bearer token with the correct resource audience; invalid/expired credentials produce 401, insufficient permission 403. Resource indicators bind tokens to the canonical MCP URI. Client ID metadata documents are recommended, pre-registration is supported, and dynamic registration is deprecated in the current revision. These are distinct from merely accepting a manually provisioned bearer token. [Current authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)

OAuth integration must publish protected-resource metadata naming an authorization server, discoverable via a `WWW-Authenticate` resource-metadata challenge or the specified well-known URI. The authorization server separately exposes OAuth metadata or OIDC discovery. Publishing a JSON document without an actual authorization/token flow is insufficient. [Authorization discovery requirements](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/authorization-server-discovery)

Tailscale Serve makes a service available within the tailnet and applies tailnet access rules; public exposure is a different facility. Therefore a local desktop client running on an authorized Tailscale-connected machine can reach the proposed endpoint. A remotely hosted client cannot be assumed to reach it merely because the user's browser can. This first design does not require public ingress. Tailnet network permission still does not identify which Baby Tracker family a request may read. [Tailscale Serve documentation](https://tailscale.com/docs/features/tailscale-serve)

Proposed private credential lifecycle, requiring design approval:

1. An authenticated user creates a named MCP connection in an app pairing flow; display its family, read-only scope, expiry, and revocation control. A short-lived, single-use pairing code may redeem into the connection credential. Never make the family chosen by an untrusted tool argument authoritative.
2. Issue a high-entropy, MCP-specific credential; store only its keyed digest. Bind it to account, family, connection, server instance, recovery epoch, read scope, and expiration. Use a distinct token namespace so `/v1/sync` cannot accept it as an ordinary app credential.
3. Reuse the existing durable security-authority event and revocation design for grant creation/revocation and validate the live account/family membership on every read. Family removal/deletion, account deletion, recovery epoch changes, expiry, or explicit disconnect must disable access. Persisting a token row alone is insufficient.
4. Do not fabricate an ordinary device/session principal to bypass the security guard. Either model a delegated MCP principal explicitly and extend the guarded authorization path, or add a defined connection identity that the authority can validate. This is a real integration task: today's principal requires device and session identity.
5. Return a precise reconnect/re-pair outcome on expiration. Select the lifetime and renewal UX explicitly; a long-lived credential should not silently inherit the phone's refresh-token authority. Audit tool name, grant identifier, latency, and result size without storing tokens or care content.

The proposed scope `family:read` is a product permission, not an existing repository scope. Scope checks must intersect the grant's fixed family and current membership; a label alone does not enforce access. For OAuth later, map the same application grant to audience-bound access tokens and keep live grant revocation effective rather than trusting unexpired JWT claims alone.

## Existing integration boundaries

| Verified repository behavior | Consequence for MCP |
| --- | --- |
| `httpapi.New` builds one `http.ServeMux` with shared middleware; auth currently has Apple exchange and app session refresh endpoints. [router](../../Server/internal/httpapi/router.go), [auth handlers](../../Server/internal/httpapi/auth.go) | Mount an optional SDK HTTP handler with explicit readiness, authentication, origin checks, and limits. Existing Apple exchange is not a complete MCP OAuth authorization server. |
| `auth.Service.Authenticate` validates instance, recovery epoch, session state, deadlines, device/session authority, and family scope. [authentication](../../Server/internal/auth/refresh.go) | Preserve these revocation properties for delegated MCP credentials; reuse underlying policy without distributing mobile session secrets. |
| `access.Principal` contains account/device/session, security revision, recovery epoch, and scope. [principal](../../Server/internal/access/types.go) | An MCP connection needs an explicit authority-compatible identity, not an unchecked account ID in context. |
| `family.Authorize` resolves active account, family slot, family, membership, role, and entitlement. Reads allow owner/caregiver/viewer. [authorization](../../Server/internal/family/service.go), [role policy](../../Server/internal/family/membership.go) | Family readers should call the established authorization path and respect viewer behavior. |
| `security.WithAuthorizedTransaction` validates the session and authority revision, sets tenant-local database context, and executes under the authorization fence. [transaction guard](../../Server/internal/security/transaction.go) | Add query operations beneath this boundary; direct ad hoc SQL from tool handlers would bypass important policy. |
| `FullSnapshot` locks the family, scans canonical records, persists snapshot metadata and entities, and then pages them. [snapshot](../../Server/internal/sync/snapshot.go) | Do not implement every history or summary call by invoking full sync snapshots. Add bounded, indexed range queries and deterministic aggregates. |

## Read-only tools and result design

Proposed tools are `list_babies`, `get_history`, and `get_summary`, all bound to the connection's family. `get_history` accepts a baby ID, bounded time range, optional record-kind filter, and an opaque continuation cursor. `get_summary` returns deterministic totals and durations over an explicit interval and timezone, with units, freshness, exclusions, and missing-data information. Summaries must account for composite feeding and ongoing/overlapping sessions rather than counting flattened legacy feeds. Do not add an LLM call to the sync server for summaries.

Mark tools read-only, idempotent, and closed-world where their SDK annotations permit. Enforce read-only behavior in services and database permissions: annotations are descriptive hints. The MCP specification requires input validation, access control, rate limiting, and output sanitization, and treats annotations as untrusted unless the server is trusted. Typed structured results should conform to their output schema; a text representation helps older clients. [Tool specification](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)

Do not expose record creation, timer control, raw SQL, exports, family membership changes, or operator commands in this first catalogue. A future write scope would need explicit product approval, independent authorization, durable idempotency, expected entity versions, and clear tool semantics for confirmation. It must reuse the canonical mutation path so MCP cannot skip compatibility rules or conflict handling.

Treat free-text notes as data, not instructions. Keep them delimited in structured fields. Default outputs should contain only fields needed for the question; exclude credentials, internal authority data, invitation secrets, and attachments. If resources are later added, they need the same authorization as tools. The SDK defaults cacheable results to public unless overridden: use private cache scope for personalized resource/list results and HTTP `Cache-Control: no-store` for sensitive responses. [SDK result caching documentation](https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/v1.8.0/docs/server.md)

## Resource budgets and verification proposal

These are starting limits to measure on the Unraid node, not proven capacity claims:

- 32 KiB MCP request bodies; at most 100 history records and 128 KiB serialized output per page; bounded free-text fields.
- At most 31 days per history query and 90 days per summary query initially; larger requests split explicitly, never silently truncate summary totals.
- Five-second end-to-end tool deadline, two in-flight calls per connection, and a small configurable global MCP concurrency cap that reserves database capacity for mobile sync. Account/family aggregate quotas prevent issuing extra connection credentials from multiplying allowances.
- Keyset pagination with cursors bound to family, baby, filters, authorization epoch, and expiry. Define consistency under concurrent edits; do not imply a frozen snapshot unless one is actually implemented.
- No subscriptions, event replay store, or per-client history cache initially. If aggregation later needs caching, bound bytes and key it by family/data revision/timezone/range while separately rechecking authorization.

Acceptance should include current and selected legacy-client discovery/list/call exchanges, unauthorized discovery/data requests, origin rejection, wrong-family baby IDs, revoked/expired grants, removal during a read, recovery/quarantine behavior, malformed/chunked oversized input, deadline cancellation, cursor tampering, and output byte caps. Verify summary results against canonical fixtures including composite feeds, DST day boundaries, deleted records, and ongoing sessions.

For capacity, compare the same mobile-sync workload with MCP disabled, idle connected clients, and bounded concurrent MCP reads. Record sync and MCP p95/p99 latency, errors, CPU, RSS, goroutines, database pool waits, SQL time, and per-family contention. A passing SDK protocol test does not establish desktop-client compatibility or single-node capacity. No tests, server implementation, production changes, or load measurements were performed for this research.

Remaining decisions: exact desktop clients and their credential/header support; pairing entry point; connection lifetime/renewal; precise summary definitions; measured node budgets. Read-only family scope and private desktop access are confirmed requirements, not remaining questions.
