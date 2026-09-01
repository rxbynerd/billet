# Billet

Billet is the **memory sidecar** of the Equestrianism suite — the
strap/buckle hardware that connects the girth to the saddle. It exposes
two tools, `save_memory` and `search_memory`, over two transports — MCP
Streamable HTTP for direct agent access, and Connect RPC
(`billet.v1.MemoryService`, speaking Connect/gRPC/gRPC-Web) for
control-plane-proxied deployments — backed by a pluggable storage
backend: an in-process ephemeral default, a local persistent `bolt`
file, or AWS Bedrock AgentCore Memory. It is written in Go, ships as a
single static binary, and is
deliberately thin: it owns the protocols, namespace binding, and cost
governance, and delegates extraction and consolidation entirely to the
configured backend. Where Chiron *investigates* and Stirrup *changes*
code, Billet *remembers*.

### Deployment models

- **Direct (default):** the MCP endpoint is reachable from the agent
  environment; Stirrup's harness calls Billet itself. Minimal moving
  parts.
- **Proxied:** only the RPC endpoint is enabled (`--rpc --mcp=false`),
  reachable solely by the control plane, which proxies tool calls to
  Billet — the agent environment gets no network path to the knowledge
  system, preserving Stirrup's no-direct-egress security posture while
  staying compatible with the rest of the Equestrianism toolset.
- Both transports can run at once (separate listeners, one shared
  backend and budget).

## Building

```sh
just build   # go build -o bin/billet ./cmd/billet
just test    # go test ./...
just vet     # go vet ./...
just lint    # golangci-lint if installed, else go vet
just ci      # everything CI runs
```

Requires Go 1.27. Without `just`: `go build -o bin/billet ./cmd/billet`.

## Usage

### The two commands

```sh
# Start the server (MCP only, loopback-only default; see the --listen note below).
billet serve

# Control-plane-proxied model: RPC only, no direct MCP surface.
billet serve --rpc --mcp=false

# Emit the resolved BilletConfig JSON without starting a server.
billet config --backend agentcore-memory --region eu-west-2 --memory-id mem-abc123
```

### Config

```json
{
  "backend": {
    "type": "agentcore-memory",
    "region": "eu-west-2",
    "memoryId": "mem-abc123",
    "credentialsRef": "secret://AWS_PROFILE"
  },
  "namespace": "prod",
  "mcp": { "enabled": true, "listen": "127.0.0.1:8140" },
  "rpc": { "enabled": false, "listen": "127.0.0.1:8141" },
  "budget": { "monthlyGbp": 50 }
}
```

Resolution order (lowest to highest precedence): documented defaults →
base config (`--config <path>`, `--config -`, or piped stdin) → explicit
flags. `backend.type` defaults to `"memory"` — an in-process, ephemeral
store with no external dependencies — so a fresh deployment never talks
to a billable cloud service by accident. `namespace` is required when
`backend.type` is `"agentcore-memory"` or `"bolt"`, and must match
`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`. `backend.path` is required when
`backend.type` is `"bolt"`: the bbolt database file to use, created if
absent (its parent directory must already exist). A `bolt` config:

```json
{
  "backend": { "type": "bolt", "path": "/var/lib/billet/billet.db" },
  "namespace": "prod",
  "mcp": { "enabled": true, "listen": "127.0.0.1:8140" },
  "rpc": { "enabled": false, "listen": "127.0.0.1:8141" }
}
```

| Flag | Config field | Default | Notes |
| --- | --- | --- | --- |
| `--mcp` | `mcp.enabled` | `true` | Serve the MCP Streamable HTTP endpoint (direct agent-environment access). |
| `--listen` | `mcp.listen` | `127.0.0.1:8140` | MCP bind address. Loopback-only by default — the endpoint is unauthenticated, so exposing it further (`--listen :8140` or a non-loopback address) is an explicit operator choice. |
| `--rpc` | `rpc.enabled` | `false` | Serve the `billet.v1.MemoryService` Connect RPC endpoint (control-plane-proxied access). |
| `--rpc-listen` | `rpc.listen` | `127.0.0.1:8141` | RPC bind address; same loopback-only reasoning as `--listen`. |
| `--namespace` | `namespace` | `default` | Long-term recall scope (AgentCore `actorId`, or a `bolt` database's per-namespace bucket); required for `agentcore-memory` and `bolt`. |
| `--backend` | `backend.type` | `memory` | `memory`, `agentcore-memory`, or `bolt`. |
| `--region` | `backend.region` | — | AWS region (`agentcore-memory`). |
| `--memory-id` | `backend.memoryId` | — | AgentCore Memory resource ID (`agentcore-memory`). |
| `--credentials-ref` | `backend.credentialsRef` | — | `secret://` reference selecting AWS credentials (`agentcore-memory`). |
| `--db-path` | `backend.path` | — | Database file path (`bolt`). |
| `--budget` | `budget.monthlyGbp` | uncapped | Rough, call-count-based cost estimate cap in GBP (not real billing — see `docs/DECISIONS.md`). |

`billet config --validate` checks the resolved config with
`BilletConfig.Validate` and exits non-zero on failure; without it, a
partial or chained config is emitted as-is so a pipeline stage can
complete it downstream. `billet config` redacts `backend.credentialsRef`
to `secret://[REDACTED]` by default; pass `--redact=false` when a
pipeline stage genuinely needs the real value to flow through to the next
stage or to `billet serve`.

At least one transport must be enabled; when both are, they must bind
distinct addresses.

### The tool surface

| Tool | Input | Output |
| --- | --- | --- |
| `save_memory` | `content: string`, `kind?: "event"\|"fact"` | `{memory_id, accepted}` |
| `search_memory` | `query: string`, `limit?: int` (default 5) | `{records: [{memory_id, content, score, created_at}]}` |

The RPC transport exposes the same operations as
`billet.v1.MemoryService.SaveMemory`/`SearchMemory`
([`proto/billet/v1/memory.proto`](proto/billet/v1/memory.proto));
Go clients import the generated stubs from
`github.com/rxbynerd/billet/gen/billet/v1/billetv1connect`. Semantics
are identical on both transports — validation, limits, budget gating,
and error policy live in one shared core (`internal/service`).

No tool or RPC takes a `namespace` or `session_id` parameter — both are
bound once at server startup from `BilletConfig`, not chosen per call by
the calling LLM (see `docs/DECISIONS.md`).

### Wiring into Stirrup

Add an `MCPServerConfig` entry to a Stirrup `RunConfig`:

```json
{
  "tools": {
    "mcpServers": [
      {
        "name": "billet",
        "uri": "https://billet.internal:8140/",
        "allowedTools": ["save_memory", "search_memory"]
      }
    ]
  }
}
```

## Documentation

- [`docs/PROPOSAL.md`](docs/PROPOSAL.md) — the original design brief.
- [`docs/DECISIONS.md`](docs/DECISIONS.md) — what changed from the
  proposal, and why.
- [`docs/security.md`](docs/security.md) / [`SECURITY.md`](SECURITY.md)
  — security posture and vulnerability reporting.
- [`AGENTS.md`](AGENTS.md) / [`CLAUDE.md`](CLAUDE.md) — orientation for
  agentic coding sessions.
- [`TODO.md`](TODO.md) — what's left, including the live AWS smoke test
  this environment couldn't run.
