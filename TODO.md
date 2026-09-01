# TODO

What's left for a future session, in rough priority order. See
`docs/DECISIONS.md` for the reasoning behind decisions referenced here.

## 1. Live smoke test of the agentcore-memory backend

Not runnable in this environment: no AWS credentials, no region, no
AgentCore Memory resource. `internal/backend/agentcore_test.go` mocks
the two AWS operations (`CreateEvent`, `RetrieveMemoryRecords`) via a
local `agentCoreAPI` interface and asserts on exact request shape, but
that only proves Billet builds the request it intends to — it says
nothing about whether AWS accepts that request or returns records
shaped the way `Search` expects.

To close this out:

1. Provision an AgentCore Memory resource with at least one long-term
   extraction strategy configured (console or Terraform — out of
   Billet's scope; Billet assumes the resource already exists and has
   strategies defined).
2. Run:
   ```sh
   billet serve --backend agentcore-memory \
     --region <region> --memory-id <memory-id> \
     --namespace <namespace> --credentials-ref secret://AWS_PROFILE
   ```
3. Call `save_memory` a few times, wait (extraction is asynchronous —
   how long is undocumented and worth recording once observed), then
   call `search_memory` and confirm records actually come back with
   sensible `content`/`score`/`created_at`.
4. In particular, verify the `RetrieveMemoryRecords.Namespace` mapping:
   `internal/backend/agentcore.go` binds `boundNamespace(namespace)` (the
   configured namespace, trimmed, plus a trailing `/`) to both
   `CreateEvent`'s `ActorId` (write) and `RetrieveMemoryRecords`'s
   `Namespace` (read), so the two stay internally consistent and one
   namespace can never prefix-match another. This is still an assumption
   about how the Memory resource's namespace templates key records by
   actor id, just now a coherent one — if the real resource's strategies
   use a templated namespace (e.g.
   `/strategies/{strategyId}/actor/{actorId}`), this mapping needs to
   change — see `docs/DECISIONS.md`, "Backend seam: memory (default) and
   agentcore-memory" and the 2026-08-30 entry.

## 2. Wire a real Stirrup MCPServerConfig at a running Billet instance

Not runnable in this environment either (would need a Stirrup harness
run, ideally launched via Hairpin per the stated preference to test this
way). Starting point — add to a Stirrup `RunConfig`:

```json
{
  "tools": {
    "mcpServers": [
      {
        "name": "billet",
        "uri": "https://billet.internal:8140/",
        "apiKeyRef": "secret://BILLET_API_KEY",
        "allowedTools": ["save_memory", "search_memory"]
      }
    ]
  }
}
```

Notes for whoever picks this up:

- `apiKeyRef` is only meaningful if Billet sits behind something that
  checks a bearer token (an ingress, a mesh sidecar) — Billet itself
  does not check one in v1 (`docs/security.md`, trust posture). Omit
  the field entirely if there's nothing to check it, rather than
  configuring a token Billet ignores.
- The URI must satisfy Stirrup's connect-time trust-model gate
  (`harness/internal/mcp/client.go`'s `validateMCPHost`): `https://` for
  a non-loopback host, or `http://` for loopback only.
- Confirm end to end: start Billet, launch a Stirrup harness run (via
  Hairpin) with the above wired in, have the agent call `save_memory`
  and `search_memory`, and confirm the tool calls actually reach Billet
  (check Billet's stderr logs and, ideally, the backend's stored state)
  rather than silently failing and falling back to no memory (Stirrup
  logs "MCP server unavailable, skipping its tools" on connect failure
  — watch for that line specifically).
- `internal/mcpserver/server_test.go`'s `TestStirrupCompatibleRawJSONRPC`
  and the manual curl-based smoke test recorded in this project's git
  history give reasonable confidence the wire protocol is correct, but
  neither replaces an actual Stirrup harness run end to end.

## 3. Real cost governance

`internal/cost.Guard` uses a fixed, made-up
`EstimatedCostPerCallGBP` and a process-lifetime call counter — not
real AgentCore billing or usage data, and not persisted across
restarts (a restart silently resets the budget). Replace with actual
AgentCore billing/usage-derived accounting once that's available
(AWS Cost Explorer API, CloudWatch usage metrics for the AgentCore
Memory resource, or similar) — see `docs/DECISIONS.md`, "Cost
governance: per-call rejection instead of an exit code".

## 4. Other gaps and deferrals made along the way

- **No caller authentication on either endpoint (MCP or RPC).**
  Deliberate v1 posture (`docs/security.md`), matching Hairpin's
  trusted-network stance — but if Billet ever needs to check a bearer
  token itself (rather than relying on ingress/mesh mTLS or the
  proxied deployment model's network isolation), that's new work, not
  a config flag that already exists.
- **No end-to-end control-plane proxy test against the RPC transport.**
  `internal/rpcserver`'s tests drive `billet.v1.MemoryService` with the
  generated connect-go client over both the Connect protocol and
  cleartext gRPC, and a manual `buf curl` smoke test is recorded in git
  history — but nothing yet exercises the actual Equestrianism control
  plane proxying a tool call to Billet. When that integration lands,
  also decide whether the control plane's infrastructure needs the
  standard gRPC health (`grpc.health.v1.Health`) or server-reflection
  services; neither is served in v1 (`buf curl` needs the local
  `--schema proto` flag for this reason).
- **No multi-tenancy.** One Billet process serves exactly one
  namespace. Multiple tenants need multiple deployments in v1; there is
  no shared-process, multi-namespace mode.
- **`cost.Guard`'s "monthly" cap has no actual month.** It's a
  process-lifetime cumulative counter; nothing rolls it over on a
  calendar boundary. Fine as a rough trip-wire, not fine as an actual
  monthly-budget enforcement mechanism — folds into item 3 above.
- **No CI workflow.** Chiron and Stirrup both have
  `.github/workflows/ci.yml` running build/vet/test/lint with SHA-pinned
  actions; this repository has no GitHub remote yet (per the project
  owner, this is a fresh local repo), so no workflow file was added.
  Add one (mirroring Chiron's) once there's a remote to run it against.
- **No `equestrianism` GitHub topic tag.** Same reason — nothing to tag
  without a remote repository yet.
- **AgentCore Memory strategy configuration is entirely out of scope
  and undocumented here.** Billet assumes a Memory resource already
  exists with extraction strategies configured (via the AWS console or
  IaC, outside this repository). Once item 1 is done, it may be worth a
  short doc section on what strategy configuration Billet was actually
  tested against, so a future deployer isn't starting from zero.
- **`search_memory` responses have no size cap of their own, on either
  transport.** Request reads are bounded (1 MiB on both transports,
  post-decompression on RPC), but a response is bounded only by the
  existing per-field caps: `limit` (≤100) × `content` (≤256 KiB) allows
  ~25.6 MiB in the worst case, entirely from content the caller (or a
  peer on the same namespace) previously stored. Flagged by the
  2026-08-31 security review as LOW: not a regression, and reaching the
  worst case requires first storing that much content through the same
  budget guard — but if Billet ever serves genuinely adversarial
  callers, a response-byte ceiling (truncating records past it) would
  belong in `internal/service` so both transports inherit it.
- **AWS region *existence* is still not probed at startup.**
  `internal/backend/agentcore.go`'s `validRegionShape` only checks that
  the configured region is shaped like an AWS region (lowercase
  alphanumeric segments joined by hyphens); it cannot confirm the region
  actually exists or is reachable without a live AWS call, which is out
  of scope in this environment (no AWS credentials — see item 1). A
  region that is shaped correctly but doesn't exist will still only fail
  on the first real AWS call, not at construction.
- **`gosec` (enabled in `.golangci.yml` as of the 2026-08-30 fix pass)
  flags `internal/cli/config.go`'s `os.Open(path)` in `loadBase`** (G304,
  "potential file inclusion via variable"). `path` is the operator-
  supplied `--config <path>` flag value, not attacker-controlled input,
  so this is very likely a false positive — but it's unrelated to the
  review findings that motivated enabling `gosec`, so it was deliberately
  left unaddressed (no `//nolint`, no code change) rather than silently
  dismissed. Revisit if a future session wants a clean `golangci-lint
  run` with no known-suppressed findings.
- **`internal/cli.runServe`'s signal-handling/shutdown path is
  untested.** Nothing exercises the `SIGTERM`/`SIGINT` → graceful
  `httpServer.Shutdown` path or the `shutdownGrace` timeout in
  `internal/cli/servecmd.go`. Testing this would need either refactoring
  `runServe` for injectable signal delivery or an out-of-process test
  that starts the real binary and sends it a signal; neither was done in
  the 2026-08-30 fix pass (explicitly out of scope — noted here rather
  than refactored for testability under time pressure).
- **`bolt` has no bound on full-scan cost or on-disk growth.**
  `Bolt.Search` decodes and scores every record in a namespace's bucket
  on every call, inside one read transaction; `Bolt.Save` never removes
  a record. Flagged by the 2026-09-01 review wave (code review H1/M1,
  security review LOW "unbounded on-disk growth") as the same underlying
  risk in two framings: unbounded query cost as a namespace grows, and
  unbounded storage growth since nothing ever evicts. Options on record
  for whoever picks this up: a bounded reverse-cursor scan from
  `Cursor().Last()` instead of a full `ForEach`; a top-k heap instead of
  sorting every candidate; or a `tx.Size()`/`bucket.Stats().KeyN`
  write-time ceiling in `Save` that rejects (or evicts) once a namespace
  gets too large. Not implemented this pass — `docs/security.md`'s
  "Storage at rest" section documents the exposure as the only
  mitigation shipped so far.
- **`bolt`'s durable, unauthenticated `save_memory` is also an
  unauthenticated disk-filling vector.** Same underlying gap as the item
  above (security review, corroborating the growth finding from a
  different angle): with no caller authentication on either transport
  (see "No caller authentication," above) and no write-time ceiling, any
  caller that can reach `save_memory` can grow `backend.path` without
  limit on whatever host runs `billet serve`. Closing the growth item
  above closes this one too; no separate control is planned.
- **`backend.path` is not normalized or constrained.**
  `NewBoltBackend` uses the path as given: no `filepath.Abs`, no `~`
  expansion, and a symlink at `backend.path` is followed with ordinary
  filesystem semantics (no `O_EXCL`, no symlink rejection). Low severity
  — `backend.path` is operator-supplied deployment configuration, not
  request input — but worth tightening if `bolt` configs ever come from
  a less-trusted source than they do today.
- **`Bolt.Close`'s block during shutdown has no bound of its own.**
  `internal/cli/servecmd.go`'s `defer closer.Close()` runs after the
  HTTP shutdown drain, but `Close` itself can still block past
  `shutdownGrace` if a slow full-scan `Search` (see the growth item
  above) is still in flight when the drain completes — there is no way
  to interrupt it. Fixing the unbounded-scan-cost item above removes
  most of this exposure; a comment recording the tradeoff was added at
  the call site rather than a behaviour change.
- **`Bolt`'s `CreatedAt` is captured outside the write transaction.**
  `Save` computes `time.Now().UTC()` before entering `db.Update`, so
  under concurrent saves a record with a lower sequence number can
  carry a later `CreatedAt` timestamp than one with a higher sequence
  number that committed first. Billet's own tiebreak in `Search` sorts
  by sequence, not `CreatedAt`, so this doesn't affect ranking — flagged
  by the 2026-09-01 review wave as low severity, method-doc note only,
  no code change planned.
