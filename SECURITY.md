# Security

## Posture

Billet is a thin protocol and adapter layer for agentic memory: it owns
the MCP tool surface, namespace binding, and cost governance, and
delegates storage and retrieval entirely to a configured backend. It
never mutates a workspace, runs no shell, and applies no edits. See
[`docs/security.md`](docs/security.md) for the full detail: namespace
isolation, fail-closed backend construction, content-logging guarantees,
and the (unauthenticated by default) trust posture of its MCP endpoint.

## Secrets

- Configuration carries `secret://` references (for example
  `secret://AWS_PROFILE`), never literal credentials;
  `BilletConfig.Validate` rejects anything else, and the rejection error
  never echoes the offending value.
- Secrets are resolved at the `internal/secret` seam (env and file
  backends) at the last moment before use. File-backed secrets whose
  permissions admit group or world access trigger a warning, not a
  failure.
- `billet config --redact` rewrites `backend.credentialsRef` to
  `secret://[REDACTED]` for a share-safe config dump.

## Network

Billet's MCP Streamable HTTP endpoint does not authenticate callers in
v1 — deploy it behind an ingress or service mesh that terminates
authenticated TLS (mTLS or equivalent), matching Hairpin's own
documented trusted-network posture. See
[`docs/security.md`](docs/security.md#trust-posture-the-mcp-endpoint-is-unauthenticated-by-default).

## Supply chain

The dependency surface is small and deliberate: stdlib, cobra (+pflag),
yaml.v3, the official `github.com/modelcontextprotocol/go-sdk`, and the
AWS SDK for Go v2 (`config`, `service/bedrockagentcore`) for the
agentcore-memory backend. No vendor AI SDKs. Justify any new dependency
in [`docs/DECISIONS.md`](docs/DECISIONS.md) before adding it.

## Reporting a vulnerability

Open a private security advisory on GitHub
(https://github.com/rxbynerd/billet/security/advisories) rather than a
public issue. Reports are acknowledged on a best-effort basis.
