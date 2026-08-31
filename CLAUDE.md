# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, and ground
rules — it is the single orientation document for agentic sessions in
this repository. The spec is `docs/PROPOSAL.md`; what changed from it
and why is `docs/DECISIONS.md`, which wins where the two disagree.

Non-negotiables worth repeating:

- No namespace or session_id parameter on `save_memory`/`search_memory`
  — both are bound once at server startup from `BilletConfig`, never
  supplied by the caller. This applies to both transports identically:
  never add such a field to `proto/billet/v1/memory.proto` either.
- Tool semantics (validation, limits, budget gating, error policy) live
  only in `internal/service` — `internal/mcpserver` and
  `internal/rpcserver` are protocol adapters and must not grow
  behaviour of their own.
- Fail closed: a configured `agentcore-memory` backend that fails to
  construct must stop `billet serve` from starting, not silently
  degrade to the `memory` backend.
- `secret://` references only in configuration — never a literal
  credential.
- `cost_summary` NDJSON events carry counters only, never call content.
- Before changing `internal/mcpserver`'s `StreamableHTTPOptions` or
  Accept-header handling, re-read `docs/DECISIONS.md`'s interop-gap
  entry and re-verify against Stirrup's actual client code
  (`harness/internal/mcp/client.go`), not just the MCP spec — the SDK's
  spec-compliant defaults are not what that client sends.
- No emojis.
