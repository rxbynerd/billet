# Billet

Billet is the **memory sidecar** of the Equestrianism suite — the
strap/buckle hardware that connects the girth to the saddle. It exposes
two MCP tools, `save_memory` and `search_memory`, over Streamable HTTP,
backed by a pluggable storage backend: an in-process ephemeral default,
or AWS Bedrock AgentCore Memory. It is written in Go, ships as a single
static binary, and is deliberately thin: it owns the protocol, namespace
binding, and cost governance, and delegates extraction and consolidation
entirely to the configured backend. Where Chiron *investigates* and
Stirrup *changes* code, Billet *remembers*.

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
# Start the MCP server.
billet serve --listen :8140

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
  "listen": ":8140",
  "budget": { "monthlyGbp": 50 }
}
```

Resolution order (lowest to highest precedence): documented defaults →
base config (`--config <path>`, `--config -`, or piped stdin) → explicit
flags. `backend.type` defaults to `"memory"` — an in-process, ephemeral
store with no external dependencies — so a fresh deployment never talks
to a billable cloud service by accident. `namespace` is required when
`backend.type` is `"agentcore-memory"`.

| Flag | Config field | Default | Notes |
| --- | --- | --- | --- |
| `--listen` | `listen` | `:8140` | MCP Streamable HTTP bind address. |
| `--namespace` | `namespace` | `default` | Long-term recall scope (AgentCore `actorId`); required for `agentcore-memory`. |
| `--backend` | `backend.type` | `memory` | `memory` or `agentcore-memory`. |
| `--region` | `backend.region` | — | AWS region (`agentcore-memory`). |
| `--memory-id` | `backend.memoryId` | — | AgentCore Memory resource ID (`agentcore-memory`). |
| `--credentials-ref` | `backend.credentialsRef` | — | `secret://` reference selecting AWS credentials (`agentcore-memory`). |
| `--budget` | `budget.monthlyGbp` | uncapped | Rough, call-count-based cost estimate cap in GBP (not real billing — see `docs/DECISIONS.md`). |

`billet config --validate` checks the resolved config with
`BilletConfig.Validate` and exits non-zero on failure; without it, a
partial or chained config is emitted as-is so a pipeline stage can
complete it downstream. `billet config --redact` rewrites
`backend.credentialsRef` to `secret://[REDACTED]`.

### MCP tools

| Tool | Input | Output |
| --- | --- | --- |
| `save_memory` | `content: string`, `kind?: "event"\|"fact"` | `{memory_id, accepted}` |
| `search_memory` | `query: string`, `limit?: int` (default 5) | `{records: [{memory_id, content, score, created_at}]}` |

Neither tool takes a `namespace` or `session_id` parameter — both are
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
