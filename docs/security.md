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
tools"). This is a current guarantee, not a roadmap item: within one
Billet instance, there is only one namespace, so there is no
cross-namespace query or write for a compromised or careless caller to
exploit — the isolation is structural, not policy-enforced.

The isolation this does *not* provide: if two different tenants need
separate memory, they need two separate Billet deployments (two
processes, two configs, likely two AgentCore Memory resources), not two
namespaces inside one process. Billet has no multi-tenancy story in v1.

## Fail closed on backend construction failure

`billet serve` builds its configured backend once, at startup, before
opening the listener. If `backend.type` is `agentcore-memory` and
construction fails — bad region, an unresolvable `credentialsRef`,
malformed config — the process logs the failure and exits non-zero. It
never falls back to the in-process `memory` backend: silently
downgrading a deployment that explicitly asked for durable storage into
one that forgets everything on restart would be a data-loss trap
disguised as uptime.

The in-process `memory` backend is also the default when `backend.type`
is unset — but that's a default *for a fresh, unconfigured deployment*,
not a fallback from a failed one. The distinction matters: choosing the
safe default is fine; silently substituting it after an explicit,
failed choice is not.

## Content logging

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
- `internal/mcpserver` does not log tool calls at all in v1: it either
  succeeds (delegating to the backend) or returns an error, which the
  MCP SDK turns into a tool-level error result sent back to the caller,
  not a server-side log line.

The `agentcore-memory` backend's request-shaping tests
(`internal/backend/agentcore_test.go`) assert on request structure sent
to AWS, which is expected to carry content (that's what `CreateEvent`
is for) — this is the one place content legitimately leaves the
process, over the AWS SDK's TLS-protected SigV4-signed connection, going
to the backend that was explicitly configured to store it.

## Trust posture: the MCP endpoint is unauthenticated by default

Billet's MCP Streamable HTTP endpoint does not authenticate or authorize
callers in v1 — matching Hairpin's own documented trusted-network
posture (`hairpin/README.md`/`docs/design.md`: "keep the Service on a
trusted network and terminate authenticated TLS at an ingress or use
mesh mTLS before exposing it outside the cluster"). Anyone who can reach
`BilletConfig.listen` can call `save_memory` and `search_memory` for the
configured namespace.

Recommended mitigation, same as Hairpin's: put Billet behind an ingress
or service mesh that terminates mTLS (or equivalent authenticated TLS)
and restricts which callers can reach it, rather than building caller
authentication into Billet itself. This keeps Billet's own surface
small and matches the rest of the suite's posture — Stirrup's
`types.MCPServerConfig.APIKeyRef` supports a bearer token if a specific
deployment needs one, but Billet does not require or default to one.

## Secrets

`backend.credentialsRef` is always a `secret://` reference
(`internal/secret`), never a literal credential; `BilletConfig.Validate`
rejects a literal. `billet config --redact` rewrites it to
`secret://[REDACTED]` for a share-safe config dump. Resolution errors
(`internal/secret/resolve.go`) never echo the raw reference or resolved
value — only the reference's shape (an env var name, a file path) — so
a misconfigured or failed lookup cannot leak a credential into a log
line or error message.

## Cost governance is not a security control

`internal/cost.Guard`'s budget cap is an operational safety net against
runaway spend, not an access control. It rejects calls once a rough,
call-count-based estimate reaches a configured cap; it does not
distinguish legitimate from abusive callers, rate-limit per caller, or
protect against denial-of-service (an unauthenticated caller who wants
to exhaust the budget and deny service to others can just do that — see
the trust-posture section above for the actual mitigation).
