# Security

This document covers Billet's security-relevant behaviour: what
isolation it guarantees, what happens on failure, what it logs, and what
trust it assumes of its network environment. See
[`SECURITY.md`](../SECURITY.md) for how to report a vulnerability.

## Namespace isolation

A Billet server binds exactly one namespace, set at startup via
`BilletConfig.namespace` (or `--namespace`) and never accepted from a
caller — `save_memory`/`search_memory` carry no namespace parameter (see
`docs/DECISIONS.md`, "No namespace or session_id parameter on the
tools"). Within one Billet instance, there is only one namespace, so
there is no cross-namespace query or write for a compromised or careless
caller to exploit at the tool-call layer.

Within the `agentcore-memory` backend specifically, namespace isolation
also has to hold against AWS's own API: `RetrieveMemoryRecords` documents
its `Namespace` parameter as a *prefix* filter, not an exact match, so an
unqualified namespace like `acme` would also match records stored under
`acme-corp` or `acme2` on a Memory resource shared by more than one
Billet deployment. `internal/backend/agentcore.go` closes this by
appending a `/` delimiter to the configured namespace (`boundNamespace`)
before using it as either AgentCore's `actorId` (on write, via
`CreateEvent`) or the `RetrieveMemoryRecords` namespace prefix (on read)
— `acme/` cannot prefix-match `acme-corp/`. `BilletConfig.Validate` also
requires the configured namespace to match
`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`, closing off the equivalent typo class
(leading/trailing whitespace, empty segments) before it ever reaches the
backend. `internal/backend/agentcore_test.go`'s
`TestNamespaceDelimiterPreventsPrefixCollision` pins this with two
namespaces where one is a strict prefix of the other.

What remains an assumption, not a tested guarantee: whether an AgentCore
Memory resource's own namespace *templates* (configured per extraction
strategy, outside Billet's scope) actually key long-term records by the
raw `actorId` Billet supplies. Billet cannot verify this without a real
AgentCore Memory resource to test against (`TODO.md`, item 1) — the
delimiter fix makes Billet's own read/write namespace values internally
coherent and collision-resistant, but does not, on its own, confirm what
AWS does with them.

The isolation this does *not* provide: if two different tenants need
separate memory, they need two separate Billet deployments (two
processes, two configs, likely two AgentCore Memory resources), not two
namespaces inside one process. Billet has no multi-tenancy story in v1.

## Fail closed on backend construction failure

`billet serve` builds its configured backend once, at startup, before
opening the listener. If `backend.type` is `agentcore-memory` and
construction fails — a malformed region, an unresolvable
`credentialsRef`, or an absent/invalid AWS credential chain — the process
logs the failure and exits non-zero. It never falls back to the
in-process `memory` backend: silently downgrading a deployment that
explicitly asked for durable storage into one that forgets everything on
restart would be a data-loss trap disguised as uptime.

Construction actively probes for the failure modes that would otherwise
only surface on the first `save_memory`/`search_memory` call (AWS
resolves credentials lazily, and doesn't validate region format itself):

- The region is checked against a cheap shape pattern (lowercase
  alphanumeric segments joined by hyphens) before any AWS call is made.
  This catches an obvious typo or a value that clearly isn't a region;
  it does **not** confirm the region actually exists or is reachable —
  that would require a live call, which is out of scope here (`TODO.md`).
- The resolved AWS credential chain is probed once
  (`aws.CredentialsProvider.Retrieve`) before construction succeeds, so a
  bad or absent credential chain fails startup immediately instead of
  being discovered silently on the first tool call.
- A `credentialsRef` that resolves to an AWS shared-config profile name
  the shared config files don't have (`SharedConfigProfileNotExistError`)
  is caught specifically and replaced with a generic message — the AWS
  SDK's own error text embeds the profile name, which would otherwise be
  the resolved secret value reaching the startup log verbatim.

The in-process `memory` backend is also the default when `backend.type`
is unset — but that's a default *for a fresh, unconfigured deployment*,
not a fallback from a failed one. The distinction matters: choosing the
safe default is fine; silently substituting it after an explicit,
failed choice is not.

## Content logging and error detail

`save_memory` content must never appear in a log line or NDJSON event
in plaintext, beyond the memory backend actually storing it (which is
the point of the tool).

- `internal/cost.Guard`'s periodic `cost_summary` NDJSON events
  (`internal/cost/cost.go`) carry only counters (`calls`,
  `estimated_gbp`, `cap_gbp`) — the `Summary`/`event` types have no field
  that could carry call content, and `TestCostSummaryNeverIncludesContent`
  pins this structurally, not just by inspection.
- `internal/cli`'s startup and shutdown log lines (`billet serving`,
  `shutting down`, backend-construction failures) log configuration
  (listen address, backend type, namespace) and error text, never tool
  call arguments.
- `internal/service` (shared by both transports) logs a tool call's
  outcome, not its content: a budget-guard rejection or a
  `Save`/`Search` failure is logged server-side via `log/slog` with the
  full error detail (which, for the `agentcore-memory` backend, can
  include AWS account IDs, role ARNs, resource ARNs, and request IDs),
  while the caller receives only a short, stable, generic message
  (`"save_memory: backend unavailable"`, `"search_memory: backend
  unavailable"`, `"budget exceeded"`) — as an MCP tool error on the MCP
  transport, or as an `UNAVAILABLE`/`RESOURCE_EXHAUSTED` status on the
  RPC transport. `TestSaveMemoryBackendErrorReturnsGenericMessage`,
  `TestSearchMemoryBackendErrorReturnsGenericMessage`, and
  `TestBudgetExceededMessageDoesNotLeakDetail` (MCP) and
  `TestBackendErrorMapsToUnavailableGeneric`,
  `TestBudgetExceededMapsToResourceExhausted` (RPC) pin this.
  Caller-caused validation errors (empty content, empty query, an
  invalid `kind`, oversized content) are unaffected and remain
  specific, since they describe the caller's own request rather than
  Billet's internals.

The `agentcore-memory` backend's request-shaping tests
(`internal/backend/agentcore_test.go`) assert on request structure sent
to AWS, which is expected to carry content (that's what `CreateEvent`
is for) — this is the one place content legitimately leaves the
process, over the AWS SDK's TLS-protected SigV4-signed connection, going
to the backend that was explicitly configured to store it.

## Trust posture: both endpoints are unauthenticated by default

Neither of Billet's endpoints — the MCP Streamable HTTP transport nor
the `billet.v1.MemoryService` Connect RPC transport — authenticates or
authorizes callers in v1, matching Hairpin's own documented
trusted-network posture (`hairpin/README.md`/`docs/design.md`: "keep the
Service on a trusted network and terminate authenticated TLS at an
ingress or use mesh mTLS before exposing it outside the cluster").
Anyone who can reach an enabled listener can call `save_memory` and
`search_memory` for the configured namespace.

Because there is no authentication layer, both listen addresses default
to loopback (`mcp.listen` `127.0.0.1:8140`, `rpc.listen`
`127.0.0.1:8141`). Exposing either beyond the local machine (`--listen
:8140`, `0.0.0.0`, or any other non-loopback address) is an explicit
operator choice, not something a fresh, unconfigured deployment does by
accident.

The two transports bind separate listeners precisely so their network
exposure can differ: in the control-plane-proxied deployment model
(`--rpc --mcp=false`), the agent environment has no network path to
Billet at all — only the control plane does — which is itself a
meaningful access-control boundary even before any ingress-level
authentication is added.

Recommended mitigation, same as Hairpin's: put Billet behind an ingress
or service mesh that terminates mTLS (or equivalent authenticated TLS)
and restricts which callers can reach it, rather than building caller
authentication into Billet itself. This keeps Billet's own surface
small and matches the rest of the suite's posture — Stirrup's
`types.MCPServerConfig.APIKeyRef` supports a bearer token if a specific
deployment needs one, but Billet does not require or default to one.

## Endpoint hardening

A handful of protections apply to both transports regardless of the
trust posture above, because "unauthenticated by default" should not
also mean "unbounded":

- **HTTP server timeouts.** Each of `billet serve`'s listeners sets
  `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout`
  explicitly (`internal/cli/servecmd.go`), so a slow or stalled client
  (deliberate or not) cannot hold a connection open indefinitely.
- **Request body and content size limits.** The Streamable HTTP handler
  sets an explicit `MaxRequestBodyBytes` (1 MiB) rather than relying on
  the SDK's current default; the RPC handler sets the equivalent
  `connect.WithReadMaxBytes` (1 MiB); and `save_memory`'s `content`
  field is separately capped at 256 KiB in the shared service, returned
  as an ordinary validation error when exceeded — the length limit is
  caller-facing information, not an internal detail.
- **CORS/DNS-rebinding protection.** Both handlers are wrapped with
  `http.CrossOriginProtection`, rejecting a cross-origin browser request
  before it reaches any tool (pinned per transport by the two
  `TestCrossOriginRequestsAreRejected` tests). The MCP SDK also
  auto-enables Host-header DNS-rebinding protection for a loopback bind
  unless explicitly disabled.
- **Unencrypted HTTP/2 is scoped to the RPC listener.** Cleartext gRPC
  requires it there (`rpcserver.Protocols()`); the MCP listener does not
  enable it.
- **Generic errors on the wire.** A backend failure (from `Save` or
  `Search`) or a budget-guard rejection never reaches the caller with its
  internal detail intact — see "Content logging and error detail" below.
  Caller-caused validation errors (empty content or query, an invalid
  `kind`, oversized content, a caller-supplied `limit` beyond the 100
  ceiling being clamped rather than rejected) remain specific, since
  they describe the caller's own request, not Billet's internals.

## Secrets

`backend.credentialsRef` is always a `secret://` reference
(`internal/secret`), never a literal credential; `BilletConfig.Validate`
rejects a literal, describing the shape of the problem rather than
echoing the rejected value — `TestValidateCredentialsRefErrorNeverEchoesValue`
pins this. `billet config` rewrites it to `secret://[REDACTED]` by
default (`--redact=false` opts out for a pipeline stage that genuinely
needs the real value), and redaction is applied before a `--validate`
failure is reported, so a future `Validate` change that echoes a value
still can't undo `--redact`'s purpose on the failure path. Resolution
errors (`internal/secret/resolve.go`) never echo the raw reference or
resolved value — only the reference's shape (an env var name, a file
path) — so a misconfigured or failed lookup cannot leak a credential into
a log line or error message. The same principle extends to the
`agentcore-memory` backend's own credential resolution: an AWS
shared-config profile name that doesn't exist is reported generically
rather than via the AWS SDK's own error text, which otherwise embeds the
profile name (see "Fail closed on backend construction failure").

## Cost governance is not a security control

`internal/cost.Guard`'s budget cap is an operational safety net against
runaway spend, not an access control. It rejects calls once a rough,
call-count-based estimate reaches a configured cap; it does not
distinguish legitimate from abusive callers, rate-limit per caller, or
protect against denial-of-service (an unauthenticated caller who wants
to exhaust the budget and deny service to others can just do that — see
the trust-posture section above for the actual mitigation). The
rejection an MCP caller sees is a generic `"budget exceeded"`, not the
configured cap or the cumulative call count — that detail is logged
server-side instead, since disclosing it to an unauthenticated caller
would hand them exactly the information needed to tune an exhaustion
attempt.
