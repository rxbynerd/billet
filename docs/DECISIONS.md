# Decisions

A running log of decisions that shaped Billet, and why. See
[`docs/PROPOSAL.md`](PROPOSAL.md) for the original design brief this
supersedes in places.

## Project name: Billet, not Girth

The proposal's placeholder name, Girth, was rejected by the project
owner (unintended sexual connotations). **Billet** was chosen instead:
it's the strap/buckle hardware that actually connects the girth to the
saddle — a plausible, specific piece of tack vocabulary with no baggage,
and, like Girth, swappable later if a better name turns up. The module
is `github.com/rxbynerd/billet`; the binary is `billet`.

## Transport: MCP server over Streamable HTTP, not connect-go/gRPC

The proposal's open question #1 asked whether Stirrup's tool-calling
loop shells out to a CLI or dials a persistent RPC sidecar. The answer,
read directly from `harness/internal/mcp/client.go` and
`harness/internal/core/factory.go` in the Stirrup repository: neither.
Stirrup's *only* mechanism for calling an external tool backend is a
remote MCP client over Streamable HTTP (JSON-RPC 2.0 over HTTP POST),
configured via `types.MCPServerConfig` (`Name`, `URI`, `APIKeyRef` as a
`secret://` ref, `AllowedTools`, `AllowedMCPHosts`) nested under
`RunConfig.Tools.MCPServers`. There is no CLI shell-out and no
connect-go/gRPC usage anywhere in Stirrup.

Billet v1 is therefore an MCP server exposing two tools — `save_memory`
and `search_memory` — served over Streamable HTTP. The proposal's
protobuf RPC sketch and its `proto/`/buf/protobuf-codegen scaffolding
are dropped entirely; there is no `proto/` directory in this
repository.

### MCP server library: `github.com/modelcontextprotocol/go-sdk`

The official Go SDK (`github.com/modelcontextprotocol/go-sdk`, current
stable v1.7.0) exists on the module proxy and implements the Streamable
HTTP server transport, JSON Schema generation from Go types via
`mcp.AddTool`, and typed tool handlers. Given a maintained, spec-current
library exists, hand-rolling the JSON-RPC surface would have meant
reimplementing session negotiation, SSE framing, and schema generation
for no benefit — so the SDK was used as-is rather than hand-rolled.

### Interop gap: the SDK's defaults reject Stirrup's actual client

Reading Stirrup's client was not enough on its own: the SDK's default
`StreamableHTTPHandler` behaviour was verified empirically (a throwaway
Go program driving `httptest.NewServer` against both) and found
**incompatible** with Stirrup's real client as written, in two ways:

1. Stirrup's client never sends an `Accept` header on its JSON-RPC POST
   requests. The SDK's handler requires `Accept` to contain both
   `application/json` and `text/event-stream` and returns
   `400 Bad Request: Accept must contain both...` when the header is
   absent.
2. Stirrup's client does a plain `json.Unmarshal` of the response body
   and never issues an `initialize` call before `tools/list`. The SDK's
   default response framing is `text/event-stream` (`event: message\ndata:
   ...`), which is not valid JSON on its own, and its stateful session
   handling expects the initialize handshake to have happened.

Billet's server therefore: (a) serves in `Stateless: true,
JSONResponse: true` mode — stateless mode synthesizes default session
state per request instead of requiring `initialize` first, and
`JSONResponse` returns a plain JSON body instead of SSE framing — and
(b) wraps the handler in a small middleware that fills in a compliant
`Accept` header when the incoming request has none. Both are pinned by
`internal/mcpserver/server_test.go`'s `TestStirrupCompatibleRawJSONRPC`,
which drives the server with a request shaped exactly like Stirrup's
client (no `Accept` header, no `initialize`, plain JSON response
expected), alongside a second test exercising the same server through
the real MCP SDK client for spec-compliant callers.

This is worth remembering for future Billet work: the SDK's out-of-the-
box defaults target the reference MCP client, and Stirrup's is a
deliberately minimal, non-fully-compliant one. Re-verify against
Stirrup's actual client code (not just the MCP spec) before changing
`internal/mcpserver`'s `StreamableHTTPOptions`.

## No namespace or session_id parameter on the tools

The proposal's RPC sketch carried `session_id` and `namespace` on every
call. Stirrup has no durable identity concept — only an ephemeral
per-run `RunID`, no user/org/tenant concept, confirmed single-tenant by
design — and no session concept beyond that run. Accepting either as a
caller-supplied parameter would mean trusting the calling LLM to pick
which tenant's memory to read or write, which is exactly the isolation
boundary Billet exists to hold.

- **`namespace`** (the long-term recall scope, mapping to AgentCore's
  `actorId`) is bound at server config/startup via `BilletConfig.namespace`.
  One Billet deployment serves exactly one namespace in v1.
- **`session_id`** (AgentCore's short-term/event scope) is likewise not
  caller-supplied: the `agentcore-memory` backend generates one session
  id per backend construction (i.e. per Billet server process lifetime)
  and uses it for every `CreateEvent`-equivalent call. This is a
  deliberate v1 simplification, not a permanent design: real
  cross-run session continuity isn't meaningful until Stirrup grows a
  session concept of its own (see TODO.md).

Final tool signatures:

```
save_memory(content: string, kind?: "event"|"fact") -> {memory_id, accepted}
search_memory(query: string, limit?: int = 5) -> {records: [{memory_id, content, score, created_at}]}
```

`SaveRequest`/`SearchRequest`/`Record` in `internal/backend` mirror this:
no namespace or session field. A concrete `Backend` (e.g.
`AgentCoreMemory`) is handed its namespace and session id once, at
construction, from `BilletConfig` — never per call.

## Billet stays separate from Paddock

`chiron/docs/PADDOCK.md` and `paddock/PLAN.md` describe a stalled,
code-less proposal for a suite-wide memory store behind Chiron's
`internal/memory.ContextStore` seam. Billet is a different project on
purpose:

- Different contract: Paddock would satisfy Chiron's `ContextStore`
  interface (session-scoped artifacts by reference, plus long-term
  `Remember`/`Recall`); Billet speaks MCP tools to Stirrup's harness.
- Different consumer: Chiron (a research CLI) vs. Stirrup (an agent
  harness).
- Anti-SaaS stance: both projects favour a thin, ownable adapter over a
  hosted memory service, but that's a shared value, not a shared
  codebase.

No code or config in this repository references Paddock, and Billet
does not depend on it. If Paddock is ever actually built, it's worth
revisiting whether Billet's `Backend` interface and Paddock's
`ContextStore` interface should converge — but that is a v2+ question,
not a v1 one.

## Cost governance: per-call rejection instead of an exit code

Chiron's `--budget` gate blocks a run before it starts and exits `4`
(`ExitBlocked`) when the estimated cost would exceed the cap — a
one-shot CLI process has an obvious point to refuse to start. Billet is
a long-running server with no equivalent "before the run" moment: by
the time a `save_memory`/`search_memory` call arrives, the server is
already up. `internal/cost.Guard` is the adapted version of the same
idea: it tracks a rolling, call-count-based cost estimate against
`budget.monthlyGbp` and rejects (as an MCP tool error, not a protocol
error — the caller can see and react to it) any call once the estimate
reaches the cap. This is explicitly a **rough** estimate
(`EstimatedCostPerCallGBP`, a fixed guess, not derived from real AWS
billing data) — see TODO.md for replacing it with real usage-derived
accounting.

## Backend seam: memory (default) and agentcore-memory

Mirrors Chiron's `internal/memory.ContextStore` seam and Hairpin's
`internal/store.Store`: define the interface once, ship a safe no-op-
shaped default, and bind exactly one real implementation. `internal/backend.Backend`
has two implementations in v1:

- `memory` (`internal/backend/memory.go`) — in-process, ephemeral, no
  external dependencies, naive token-overlap search. This is the
  default (`BilletConfig.backend.type` defaults to `"memory"`) so a
  fresh Billet deployment never talks to a billable cloud service by
  accident, and it's what the test suite runs against.
- `agentcore-memory` (`internal/backend/agentcore.go`) — AWS Bedrock
  AgentCore Memory, using the AWS SDK for Go v2's
  `github.com/aws/aws-sdk-go-v2/service/bedrockagentcore` package
  (confirmed to exist on the module proxy at v1.43.0, so no hand-rolled
  SigV4 client was needed). `save_memory` maps to `CreateEvent`;
  `search_memory` maps to `RetrieveMemoryRecords`.

`CreateEvent`'s payload is JSON (`PayloadTypeMemberJson`), not
conversational (`PayloadTypeMemberConversational`): `SaveRequest`
carries no speaker role, and AgentCore extracts JSON payloads into
long-term memory the same way it extracts conversational ones, so
inventing a role to fit the conversational shape would add complexity
for no benefit.

`RetrieveMemoryRecords`'s `Namespace` filter is set to Billet's
configured namespace directly, on the assumption that the AgentCore
Memory resource's namespace templates use the raw actor id as-is. This
is unverified against a real AgentCore Memory resource (see TODO.md) —
real namespace templates may need a different mapping once tested.

The `agentcore-memory` backend cannot be live-tested in this
environment (no AWS credentials); its tests mock the two AWS operations
directly through a small `agentCoreAPI` interface (`CreateEvent` and
`RetrieveMemoryRecords`) rather than faking HTTP or SigV4 signing, and
assert on exact request shape. This exercises Billet's own
request-building logic, which is what can actually have a bug; it does
not exercise the AWS SDK's transport, which is out of scope to
re-test.

## Fail closed on backend construction failure

If `backend.type` is `agentcore-memory` and construction fails (bad
region, unresolvable `credentialsRef`, malformed config), `billet serve`
exits non-zero rather than falling back to the `memory` backend. Silently
downgrading to an ephemeral in-process store when a durable backend was
explicitly requested would be a data-loss trap disguised as
availability.
