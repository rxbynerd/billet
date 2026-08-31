# Billet — agent orientation

Billet is the Equestrianism suite's memory sidecar: it exposes
`save_memory` and `search_memory` over two transports — MCP Streamable
HTTP for direct Stirrup access, and Connect RPC (`billet.v1.MemoryService`)
for control-plane-proxied deployments — backed by a pluggable storage
backend. It is deliberately thin — it owns the protocols, namespace
binding, and cost governance, and delegates extraction/consolidation
entirely to the backend. Tool semantics live once, in
`internal/service`; the transport packages only adapt them.

Read `docs/PROPOSAL.md` for the original brief and `docs/DECISIONS.md`
for what changed from it and why — DECISIONS.md wins where the two
disagree. `docs/security.md` documents the current security posture
(namespace isolation, fail-closed backend construction, content-logging
guarantees, trust posture).

## Build and verify

```sh
just build   # go build -o bin/billet ./cmd/billet
just test    # go test ./...
just vet     # go vet ./...
just lint    # golangci-lint if installed, else go vet
just ci      # everything CI runs
```

## Per-package map

| Package | Role |
| --- | --- |
| `cmd/billet` | Entrypoint; `os.Exit(cli.Execute())` and nothing else. |
| `internal/cli` | Cobra command tree (`serve`, `config`), flag→config resolution, and the composition root: the only place environment is read, the backend is selected and constructed, and the server is started. |
| `internal/config` | `BilletConfig`: the single declarative config. JSON/YAML, flag binding, base+overlay merge semantics for pipelines, validation (backend type enum, the agentcore-memory namespace/region/memoryId requirement, the secret:// rule). |
| `internal/backend` | The `Backend` seam (`Save`/`Search`, namespace and session bound at construction, not per call) plus its two v1 implementations: `Memory` (in-process, ephemeral, dependency-free, the default and the test-suite backend) and `AgentCoreMemory` (AWS Bedrock AgentCore Memory, via the AWS SDK for Go v2). |
| `internal/service` | The transport-neutral core of both tool operations: validation, budget gating, limit clamping, and the generic caller-facing error policy. Both transports adapt this one implementation; tool semantics change here, never in a transport package. |
| `internal/mcpserver` | The MCP transport: wires `save_memory`/`search_memory` onto `github.com/modelcontextprotocol/go-sdk`'s Streamable HTTP server. See `docs/DECISIONS.md` for the Stirrup-client interop fix (Stateless+JSONResponse mode, Accept-header defaulting) — re-verify against Stirrup's actual client before changing `StreamableHTTPOptions`. |
| `internal/rpcserver` | The RPC transport: `billet.v1.MemoryService` via connect-go (Connect, gRPC, and gRPC-Web protocols), for control-plane-proxied deployments. Cleartext gRPC needs the hosting `http.Server` to use `rpcserver.Protocols()`. |
| `proto/`, `gen/` | The `billet.v1` protobuf contract and its committed, buf-generated Go stubs (`just proto` regenerates; plugins are pinned as go.mod tool directives). `gen/` is public on purpose — the control plane imports the client stubs from it. |
| `internal/cost` | The v1 cost guard: a rough, rolling call-count-based estimate checked before every tool call, plus periodic content-free `cost_summary` NDJSON events on stderr. Not real AgentCore billing (see TODO.md). |
| `internal/secret` | `secret://` resolver (env, file backends) — the same grammar and safety behaviour as Chiron's, ported rather than imported since Billet has no dependency on Chiron. |

## Ground rules

- `internal/backend.Backend` implementations take no namespace or
  session parameter on `Save`/`Search` — those are bound at
  construction from `BilletConfig`. Do not add them back as call
  parameters; that would let the calling LLM choose which tenant's
  memory it touches (see `docs/DECISIONS.md`). The same rule holds on
  the wire for both transports: no namespace/session field on the MCP
  tools or in `proto/billet/v1/memory.proto`.
- Tool semantics (validation, limits, budget gating, error policy) live
  only in `internal/service`; the transport packages are protocol
  adapters and must not grow behaviour of their own, or the two
  transports drift.
- Billet fails closed on backend construction failure: if
  `agentcore-memory` is configured and fails to construct, `billet
  serve` must exit non-zero, never fall back to the `memory` backend.
- `secret://` references only — no literal credentials in config, ever.
  `BilletConfig.Validate` and `internal/secret` enforce this; keep it
  that way in any new config surface.
- `cost_summary` NDJSON events (`internal/cost`) must never carry call
  content — only counters. If you add a field to `Summary`, ask whether
  it could ever hold user-supplied content before adding it.
- Keep commits in logical units; explain rationale in the message body.
- No emojis in code, docs, or commit messages.

## Test infrastructure conventions

- `internal/backend`'s `agentcore-memory` tests mock the AWS operations
  through the local `agentCoreAPI` interface (`CreateEvent`,
  `RetrieveMemoryRecords`) — never real AWS, never a fake HTTP/SigV4
  transport. There is no AWS account available in this environment; see
  TODO.md for the outstanding live smoke test.
- `internal/mcpserver`'s tests drive the server two ways: through the
  real `github.com/modelcontextprotocol/go-sdk` client (spec-compliant
  path) and through raw JSON-RPC HTTP shaped exactly like Stirrup's
  actual client (no `Accept` header, no `initialize`, plain JSON
  response expected). Keep both — they pin different things.
- `internal/rpcserver`'s tests likewise drive the server two ways:
  through the generated connect-go client over HTTP/1.1 (Connect
  protocol) and over cleartext HTTP/2 with `connect.WithGRPC()` (the
  gRPC path a control plane takes with no TLS in front of Billet).
  Keep both.
- Table-test loop variables are named `tt`.
