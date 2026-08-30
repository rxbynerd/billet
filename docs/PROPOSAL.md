# Agentic memory for Equestrianism — build proposal

> **Historical record — read [`docs/DECISIONS.md`](DECISIONS.md) first.**
> This is the original design brief, kept intact for context. Several of
> its assumptions were checked against the real Stirrup codebase during
> implementation and turned out to be wrong; two further decisions were
> made by the project owner that supersede parts of this document. In
> particular:
>
> - The placeholder name **Girth** was rejected (unintended
>   connotations) in favour of **Billet** — the strap/buckle hardware
>   that actually connects the girth to the saddle. The repository is
>   `rxbynerd/billet`, not `rxbynerd/girth`.
> - The v1 transport is an **MCP server over Streamable HTTP**, not
>   connect-go/gRPC — Stirrup has no gRPC tool-calling path at all. The
>   "Proposed RPC surface" protobuf sketch below is superseded; see
>   DECISIONS.md for the actual tool surface.
> - `session_id` and `namespace` are **not** per-call RPC parameters;
>   both are bound at server config/startup. See DECISIONS.md.
> - Go 1.26 (below) is corrected to whatever `go version` reports in the
>   build environment (1.27 at time of writing).
>
> Everything else below — the non-goals, design tenets, cost-governance
> framing, and safety posture — still holds for v1.

## Why this exists

Stirrup's tool-calling loop wants two primitives — `memory_save` and
`memory_search` — so an agent can persist and recall facts across
turns and sessions. That surface is deceptively small: underneath it
sits extraction (deciding what's worth remembering), consolidation
(merging new facts with old, resolving contradictions, temporal
validity), retrieval strategy, and governance. That is genuinely hard,
actively contested ML/infra work — the open-source and vendor
landscape (Mem0, Zep/Graphiti, Letta, LangMem, Cognee) is still
fighting over extraction and consolidation quality, with no settled
winner.

Equestrianism's own precedent for this situation is Chiron: it does
not reimplement Google's Deep Research reasoning, it wraps it — owning
the ops (budget gates, resumability, structured output, exit codes)
and delegating the hard reasoning entirely. Girth should do the same
for memory: own the protocol, safety posture, and backend
pluggability; delegate extraction and consolidation to a backend built
for it.

## Non-goals (explicit, do not build)

- No custom embedding model, extraction pipeline, or consolidation
  logic in v1. If Girth ever owns this, it's a deliberate v2+ decision
  behind a `self-managed` backend adapter, not a v1 default.
- Not built on Amazon Bedrock Knowledge Bases — that's a document-sync
  RAG store (chunk, embed, retrieve) with no per-turn extraction or
  consolidation. Adopting it would mean building the hard part on top
  of it anyway, which defeats the point.
- Not a general-purpose vector database. Girth is a thin protocol and
  adapter layer, not infrastructure for arbitrary retrieval workloads.

## Design tenets (carried over from the rest of the suite)

- **Composability** — backend choice is a config swap, not a code
  change, same as Stirrup's `RunConfig` and Chiron's `ResearchConfig`.
- **No literal secrets** — API keys resolved via `secret://` refs
  only, matching Chiron's `--api-key-ref` convention.
- **Cost-aware by default** — matching Chiron's `--budget` gate, given
  the v1 backend's non-trivial billing surface (see Cost governance).
- **Research-only / mutation-only lines stay clean** — Girth doesn't
  touch a workspace or run a shell; it's a memory sidecar, full stop.
- **Structured, machine-readable output** — NDJSON events on stderr,
  a `RunResult`-style envelope on `-o json`, same shape as Chiron.

## v1 scope

1. ~~A small Go service exposing two RPCs over connect-go (JSON/gRPC on
   one h2c port, matching Hairpin's `JobService` pattern):~~ —
   superseded: Stirrup dials tools over remote MCP (Streamable HTTP),
   not connect-go/gRPC. Billet v1 is an MCP server with two tools
   instead; see DECISIONS.md.
   - ~~`SaveMemory(session_id, content, kind?) -> memory_id`~~ →
     `save_memory(content, kind?) -> {memory_id, accepted}` (no
     `session_id` parameter — bound at server config/startup)
   - ~~`SearchMemory(session_id | namespace, query, limit?) -> [records]`~~
     → `search_memory(query, limit?) -> {records}` (no `namespace`
     parameter, for the same reason)
2. A backend adapter interface with exactly one implementation in v1:
   AWS Bedrock AgentCore Memory.
3. Declarative config (~~`GirthConfig`~~ `BilletConfig`), `secret://`
   resolution, and a ~~`girth config`~~ `billet config` subcommand that
   emits resolved JSON without running — mirroring `stirrup run-config`
   / `chiron research-config` so pipelines can compose it the same way.
4. A cost/usage report on every response (see Cost governance).

## Proposed RPC surface (sketch — refine during implementation)

> **Superseded — see DECISIONS.md.** Stirrup has no connect-go/gRPC
> tool-calling path; its only external-tool mechanism is a remote MCP
> client over Streamable HTTP. Billet v1 ships two MCP tools
> (`save_memory`, `search_memory`) instead of this protobuf service, and
> there is no `proto/` directory or buf module in this repository. Kept
> below for historical context only.

```protobuf
// proto/girth/v1/girth.proto
syntax = "proto3";
package girth.v1;

service MemoryService {
  rpc SaveMemory(SaveMemoryRequest) returns (SaveMemoryResponse);
  rpc SearchMemory(SearchMemoryRequest) returns (SearchMemoryResponse);
}

message SaveMemoryRequest {
  string session_id = 1;   // scopes short-term context
  string namespace = 2;    // scopes long-term recall (e.g. per-user, per-repo)
  string content = 3;      // raw turn content or an explicit fact
  string kind = 4;         // "event" | "fact" — optional hint, backend may ignore
}

message SaveMemoryResponse {
  string memory_id = 1;
  bool accepted = 2;       // true even if extraction is async/deferred
}

message SearchMemoryRequest {
  string namespace = 1;
  string query = 2;
  int32 limit = 3;         // default 5, matches Bedrock KB's numberOfResults convention
}

message SearchMemoryResponse {
  repeated MemoryRecord records = 1;
}

message MemoryRecord {
  string memory_id = 1;
  string content = 2;
  float score = 3;
  string created_at = 4;
}
```

## Backend adapter interface (Go sketch)

```go
// internal/backend/backend.go
package backend

type Backend interface {
    Save(ctx context.Context, req SaveRequest) (memoryID string, err error)
    Search(ctx context.Context, req SearchRequest) ([]Record, error)
}
```

One adapter per backend, selected by `GirthConfig.backend.type`. This
is the seam that keeps the vendor decision reversible.

## v1 backend target: AWS Bedrock AgentCore Memory

Not Knowledge Bases — AgentCore Memory is the AWS product actually
shaped like agentic memory: short-term memory for turn-by-turn
context, long-term memory that automatically extracts and stores
insights across sessions, and a self-managed strategy option if Girth
ever wants to own extraction later without switching backends.

| Girth RPC       | AgentCore Memory operation                          |
| ---------------- | ---------------------------------------------------- |
| `SaveMemory`      | `CreateEvent` (raw turn) — long-term extraction runs asynchronously in the background |
| `SearchMemory`     | `RetrieveMemoryRecords` — semantic search over extracted long-term records |

Reference: <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory.html>,
<https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory-types.html>

## v2+ backend candidates (deferred, not v1)

For teams that want zero cloud dependency, matching Stirrup's
k8s-native, run-it-yourself posture:

- **Letta (MemGPT)** — self-editing, OS-inspired memory hierarchy.
- **Mem0 OSS** — the most widely adopted open-source option; graph
  features gated behind a paid tier if that matters later.
- **Cognee** — open-source graph memory with MCP integration already
  built in.

Each is a second `Backend` implementation behind the same interface —
no change to Girth's RPC surface or to Stirrup's calling code.

## Config & secrets

```json
{
  "backend": {
    "type": "agentcore-memory",
    "region": "eu-west-2",
    "memoryId": "...",
    "credentialsRef": "secret://AWS_PROFILE"
  },
  "listen": ":8140",
  "budget": { "monthlyGbp": 50 }
}
```

Layering follows Stirrup's precedent: documented defaults → base
config (`--config` file or piped stdin) → explicit flags.

## Cost governance

AgentCore's billing is not one rate — components (Runtime, Memory,
Gateway, Observability) bill independently per-session, per-request,
or per-record, and idle session accumulation and observability
charges are common surprises with no built-in cap. Girth should apply
a Chiron-style guard rail:

- `--budget <gbp>` blocks new sessions once a rolling estimate exceeds
  the cap, same exit-code convention as Chiron (`4` = blocked
  pre-spend).
- Emit a `cost_summary` NDJSON event per session, not just per call —
  memory calls are frequent and per-call estimates are not
  meaningful in isolation.

## Safety / trust posture

Write a `docs/security.md` and `SECURITY.md` before the first release,
matching Stirrup and Hairpin. At minimum, document:

- Namespace isolation guarantees (can one session's memory leak into
  another's search results?).
- What happens on backend auth failure — fail closed, not silently
  skip memory.
- Whether `SaveMemory` content is logged anywhere in plaintext, given
  it may carry user-provided facts.

## Repo & build conventions

- ~~Go 1.26~~ — corrected to the Go version actually installed in the
  build environment (1.27) at implementation time; see DECISIONS.md.
- Single static binary, `Justfile` with `build` / `test` / `vet` /
  `lint` / `ci` targets. ~~`proto-lint` (`buf` for proto)~~ — dropped:
  there is no protobuf in this design (see the RPC surface note above).
- `docs/PROPOSAL.md` (this file, cleaned up), `docs/DECISIONS.md` as a
  running log, `AGENTS.md` / `CLAUDE.md` for orientation.
- Apache-2.0, matching the rest of the suite.
- `equestrianism` GitHub topic tag.

## Open questions for the build session

These need answers from reading Stirrup's actual tool-calling
internals (not just its README) before implementation starts:

1. **Invocation shape** — does Stirrup's tool-calling loop shell out
   to a CLI per call, or dial a persistent RPC sidecar? This decides
   whether Girth needs a CLI wrapper around the RPC client in
   addition to the service itself.
2. **Session vs. namespace scoping** — how does Stirrup identify
   "this session" and "this durable identity" today (if at all)? This
   maps directly onto `session_id` vs `namespace` above.
3. **Where Girth runs** — local sidecar process per `stirrup harness`
   run, or a shared service alongside Hairpin's control plane?
4. **Multi-tenancy** — does one Girth deployment serve many Stirrup
   users/orgs, and if so what isolates their `namespace`s?

## Reference research

- AWS, *Add memory to your Amazon Bedrock AgentCore agent* —
  <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory.html>
- AWS, *Memory types* —
  <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory-types.html>
- AWS, *AgentCore is now generally available* —
  <https://aws.amazon.com/about-aws/whats-new/2025/10/amazon-bedrock-agentcore-available>
- Atlan, *Best AI Agent Memory Frameworks in 2026* —
  <https://atlan.com/know/best-ai-agent-memory-frameworks-2026/>
- Vectorize, *Best AI Agent Memory Systems in 2026* —
  <https://vectorize.io/articles/best-ai-agent-memory-systems>
- Mem0, *State of AI Agent Memory 2026* —
  <https://mem0.ai/blog/state-of-ai-agent-memory-2026>
- CloudBurn, *Amazon Bedrock AgentCore Pricing: 12 Components
  Breakdown* — <https://cloudburn.io/blog/amazon-bedrock-agentcore-pricing>
