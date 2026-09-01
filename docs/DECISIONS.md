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

> Partially superseded by the 2026-08-31 "two transports" entry below:
> MCP remains the default, Stirrup-facing transport, but a Connect RPC
> transport was added for control-plane-proxied deployments, and the
> proposal's protobuf contract was reinstated in adapted form.

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

## 2026-08-30: security/coverage fix pass

Three review agents (code, security, test-coverage) audited the v1 build
before it had ever run against real traffic or real AWS. This entry
records what changed and, briefly, why; `docs/security.md` and
`SECURITY.md` describe the resulting behaviour, not this pass.

**Namespace isolation was a documented claim the code didn't hold.**
`RetrieveMemoryRecords`'s `Namespace` parameter is a *prefix* filter per
the AWS SDK's own doc comment, not an exact match — so namespace `acme`
silently matched `acme-corp`, contradicting `docs/security.md`'s
"structural" isolation claim whenever two deployments shared a Memory
resource with prefix-colliding namespaces. `internal/backend/agentcore.go`
now binds a single delimited namespace value (`boundNamespace`, appending
`/`) and uses it for both `CreateEvent`'s `ActorId` (write) and
`RetrieveMemoryRecords`'s `Namespace` (read), so the two call sites can
never drift apart, and one namespace can never be a live prefix of
another. `BilletConfig.Validate` now also requires the namespace to match
`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`, closing the whitespace-typo variant of
the same gap before it reaches the backend at all. What this does *not*
newly establish: whether a real AgentCore Memory resource's namespace
templates actually key records by the raw `actorId` Billet sends — still
unverified without live AWS access (`TODO.md`, item 1).

**`Validate`'s `credentialsRef` error echoed the value it was
rejecting**, directly contradicting `SECURITY.md`'s claim that the error
"never echoes the offending value" (the general pattern was already
correct in `internal/secret/resolve.go`; this one call site in
`internal/config/config.go` was not). The error now names the shape of
the problem, not the value.

**Fail-closed backend construction didn't cover every way "closed"
should mean "refuses to start."** `awsconfig.LoadDefaultConfig` resolves
credentials lazily and never validates a region string, so a bad region
or an absent credential chain used to let `billet serve` start
successfully and then silently drop every call. Construction now probes
the resolved credential chain once (`aws.CredentialsProvider.Retrieve`)
and checks the region against a cheap shape pattern before opening the
listener. Region *existence* is still not probed — that needs a live AWS
call, which is out of scope here; the claim in `docs/security.md` is
scoped accordingly rather than left overstated. Separately, a
`credentialsRef` that resolves to a nonexistent AWS shared-config profile
used to propagate the AWS SDK's own error text — which embeds the
profile name, i.e. the resolved secret value — straight into the startup
log; that specific error type is now caught and replaced with a generic
message.

**The MCP endpoint was unbounded in several ordinary ways for an
unauthenticated service.** `billet serve`'s `http.Server` now sets
explicit `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`
(no slowloris-shaped connection can sit open indefinitely); the
Streamable HTTP handler sets an explicit `MaxRequestBodyBytes` instead of
relying on the SDK's default, and `save_memory`'s `content` field has its
own 256 KiB ceiling, clamped rather than merely documented; the handler
is wrapped in `http.CrossOriginProtection` (the SDK's own
`StreamableHTTPOptions.CrossOriginProtection` field is deprecated in
favour of external wrapping, per the SDK's v1.7.0 doc comment); and
`DefaultListen` changed from `:8140` to `127.0.0.1:8140`, so exposing the
unauthenticated endpoint beyond the local machine is an explicit
`--listen` choice rather than the out-of-the-box behaviour.

**Backend and budget errors reached the MCP caller verbatim.** A failing
`agentcore-memory` call could return AWS account IDs, role ARNs, resource
ARNs, and request IDs to whoever could reach the (unauthenticated)
endpoint; a budget rejection disclosed the exact cap and cumulative call
count, handing an attacker the exact tuning information needed to exhaust
it quietly. `internal/mcpserver/server.go` now returns a short, stable,
generic message for both cases (`"save_memory: backend unavailable"`,
`"search_memory: backend unavailable"`, `"budget exceeded"`) and logs the
full detail server-side via `log/slog`. Caller-caused validation errors
(empty content/query, invalid `kind`, oversized content) are deliberately
unchanged — they describe the caller's own request, not Billet's
internals, so there is nothing to protect by genericising them.

**`search_memory`'s `limit` had no upper bound.** A value near or above
`math.MaxInt32` silently wrapped to a negative `TopK`/`MaxResults` once
the `agentcore-memory` backend converted it to `int32`. `limit` is now
clamped to `[1, 100]` in the MCP tool handler before it reaches either
backend, with a second, backend-local clamp in
`internal/backend/agentcore.go`'s `Search` as defense in depth for any
future non-MCP caller of `Backend`.

**`billet config`'s `--redact` defaulted to false and ran after
`Validate`.** A stray `billet config` in a terminal or log could print a
live `credentialsRef` in cleartext, and a `--validate` failure was
reported before redaction had a chance to help. `--redact` now defaults
to `true` (this deliberately diverges from Stirrup's `run-config
--redact`, which defaults to `false` — Billet's command is used more
often for one-off inspection than pipeline composition, and the cost of
an accidental credential-reference disclosure outweighs the convenience
of not typing `--redact=false`), and redaction now runs before
`Validate`. A redacted `credentialsRef` (`secret://[REDACTED]`) still
satisfies `Validate`'s `secret://`-shape check, so this does not change
what `--validate` accepts.

**`mcpserver.New`'s "budget must not be nil" was undocumented as
enforced.** A nil `*cost.Guard` now defaults to an uncapped guard instead
of panicking on first use — cheap defensive coding for a documented
precondition nothing previously checked.

**`gosec` is now enabled** in `.golangci.yml`. It flagged three false
positives in test fixtures (`secret://`-prefixed strings pattern-matching
as "hardcoded credentials"; a deliberately-0644 test fixture proving the
file-permission warning it triggers), suppressed inline with `//nolint`
and a reason, plus a signed-to-unsigned integer conversion in a new
concurrency test, fixed by using `atomic.Uint64` instead of a manual
`int64` counter. One finding — `internal/cli/config.go`'s `--config
<path>` flag reading an operator-supplied local file path (G304,
"potential file inclusion via variable") — is unrelated to this pass's
findings and was deliberately left unaddressed; see `TODO.md`.

## 2026-08-31: Two transports — direct MCP and control-plane-proxied RPC

The original transport decision (above) dropped the proposal's
connect-go/gRPC sketch entirely because Stirrup's only external-tool
mechanism is a remote MCP client. That answered "how does Stirrup call
Billet?" but silently fixed a second, separate question: "who is allowed
to reach Billet's network endpoint?" Serving MCP directly means the
Stirrup agent environment itself needs network access to Billet — the
agent is the client. The rest of the Equestrianism toolset instead
expects a control plane to proxy tool calls to the knowledge system over
RPC, so the agent environment never gets a network path to it.

Billet now supports both deployment models rather than forcing the
choice:

- **Direct (default):** the MCP Streamable HTTP endpoint
  (`mcp.enabled`, default true, `mcp.listen` default `127.0.0.1:8140`)
  is reachable from the agent environment and Stirrup calls it as
  before. This showcases the minimal-integration path.
- **Proxied:** the Connect RPC endpoint (`rpc.enabled`, default false,
  `rpc.listen` default `127.0.0.1:8141`) serves
  `billet.v1.MemoryService` (Connect, gRPC, and gRPC-Web protocols via
  connect-go), and the control plane proxies `save_memory`/
  `search_memory` to it; `mcp.enabled: false` removes the direct
  surface entirely. This highlights Stirrup's
  no-direct-network-access-for-agents security posture while staying
  compatible with the rest of the toolset.
- Both can be enabled at once (dev/compat); they bind separate
  listeners so the two surfaces can face different networks, and they
  share one `internal/service` core and one cost guard, so semantics
  and budget are identical regardless of the path a call takes.

Consequences of note:

- The proposal's protobuf scaffolding is reinstated in adapted form:
  `proto/billet/v1/memory.proto`, generated via buf into the public
  `gen/` tree (committed, so builds need neither buf nor network;
  plugins pinned as go.mod tool directives; `just proto` regenerates).
  `gen/` is public so the control plane can import the client stubs.
  Unlike the proposal's sketch, the contract carries no `session_id` or
  `namespace` field — the no-caller-supplied-identity rule (above)
  applies to both transports identically.
- Tool semantics moved from `internal/mcpserver` into
  `internal/service`; the transport packages are pure protocol
  adapters. Error policy maps per transport: caller-caused validation
  errors are MCP tool errors / `INVALID_ARGUMENT`; budget rejections
  are `"budget exceeded"` / `RESOURCE_EXHAUSTED`; backend failures are
  the generic unavailable message / `UNAVAILABLE`.
- Cleartext gRPC (no TLS in front of Billet) uses Go 1.24+'s
  `http.Protocols` unencrypted-HTTP/2 support on the RPC listener —
  the x/net h2c package is deprecated in favour of it. The MCP listener
  deliberately does not enable unencrypted HTTP/2; nothing needs it
  there.
- The top-level `listen` config key and single-listener `serve` wiring
  were replaced by the symmetric `mcp`/`rpc` blocks. The strict config
  decoder makes a pre-change config fail loudly (pinned by
  `TestDecodeLegacyListenKeyRejected`); a compat alias was considered
  and rejected since the repository is pre-release with no external
  deployments.
- `TransportConfig.Enabled` serialises without `omitempty`, so a piped
  `billet config` output carries `"enabled": false` explicitly instead
  of being re-defaulted to true by a downstream stage.

## 2026-09-01: `bolt` backend — a persistent, no-AWS local store

`agentcore-memory` is the only durable option in v1, and it requires an
AWS account — unavailable in this environment and an unwelcome
dependency for anyone who wants persistence without cloud billing.
`internal/backend.Bolt` (`internal/backend/bolt.go`, using
`go.etcd.io/bbolt`, pure Go so `CGO_ENABLED=0` builds are unaffected)
fills that gap: a single local file, no network, no account.

**Namespace binds to a per-namespace bucket, not a per-deployment
file.** `NewBoltBackend(path, namespace)` derives the bucket
`"records/" + namespace` inside the database at `path`, mirroring
`agentcore-memory`'s `boundNamespace` construction-time binding (see
"No namespace or session_id parameter on the tools", above) rather than
inventing a different mechanism. This was a gap in the first landed
version — a single fixed bucket meant any two `BilletConfig`s pointed at
the same file saw each other's memories regardless of namespace — closed
in the same pass that added it, per a review wave's finding. `Validate`
now requires a namespace for `backend.type: bolt`, matching the
`agentcore-memory` requirement and the same `namespacePattern` shape
check, since the reasoning is identical: an unqualified or malformed
namespace is a silent isolation gap.

**Search skips and warns on a corrupt record rather than failing the
whole query.** The first version aborted the entire `View` transaction
on the first record that failed to decode, which meant one torn write
(or a future incompatible on-disk schema) could permanently disable
`search_memory` for a namespace while `save_memory` kept accepting
writes underneath it — availability asymmetry nobody chose on purpose.
`Bolt.Search` now counts and skips undecodable records (`v == nil`, a
key that isn't the expected 8 bytes, or a JSON decode failure) and logs
once per call via `slog.Warn` with the count and the first offending
key. This trades strict correctness (a decode failure should in
principle mean something is wrong) for availability (the store keeps
serving everything it can read) — the same trade this project already
makes by preferring a rough cost estimate over exact billing, and by
scoring token overlap instead of true semantic search in the two local
backends.

**A pre-existing file with looser-than-0600 permissions is rejected,
not silently reused.** `bbolt.Open`'s mode argument only applies to a
newly created file (`os.OpenFile` semantics); an already-existing file
at `backend.path` keeps whatever permissions it had. `NewBoltBackend`
now `os.Stat`s the path first and fails construction if a pre-existing
file allows group or other access, rather than opening it as-is. This is
deliberately stricter than `internal/secret/resolve.go`'s precedent for
`secret://` file backends, which only warns on loose permissions and
proceeds — accepted there because a misconfigured secret file is the
operator's own credential to lose, but a bolt database holds every
memory this Billet instance will ever save, in plaintext, so silently
trusting an inherited or restored file's permissions was judged too
risky to only warn about.

Other choices carried over unremarked from the first version, recorded
here for completeness: `bbolt.Open`'s lock `Timeout` is 1 second, long
enough to distinguish "another `billet serve` has this file open" from
a hang, short enough not to make a misconfigured second instance wait
noticeably; bbolt's `NoSync` option is not exposed — Billet takes the
durability bbolt gives it by default rather than trading it for write
throughput; and `Bolt`'s `Close` method is discovered by the
composition root (`internal/cli/servecmd.go`) via an `io.Closer` type
assertion rather than being added to `Backend` itself, since `Memory`
and `AgentCoreMemory` have nothing to release and adding an unused
method to the interface would make every future `Backend` carry a
no-op.

**Deferred, not fixed this pass** (see `TODO.md` for the full writeup):
unbounded on-disk growth and the full-`Search` scan cost that compounds
it; the same growth exposure reframed as an unauthenticated durable
write sink; `backend.path` not being normalized (`filepath.Abs`, `~`
expansion, symlink handling); and `Close`'s block during shutdown having
no bound of its own past the drain's grace period.
