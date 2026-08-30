# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, and ground
rules — it is the single orientation document for agentic sessions in
this repository. The spec is `docs/PROPOSAL.md`; what changed from it
and why is `docs/DECISIONS.md`, which wins where the two disagree.

Non-negotiables worth repeating:

- No namespace or session_id parameter on `save_memory`/`search_memory`
  — both are bound once at server startup from `BilletConfig`, never
  supplied by the caller.
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
