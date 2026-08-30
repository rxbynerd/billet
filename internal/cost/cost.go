// Package cost implements Billet's v1 cost governance: a rough, rolling
// call-count-based estimate of AgentCore spend, checked before every
// save_memory/search_memory call. There is no real AgentCore billing
// integration in v1 (see TODO.md) — the estimate exists to give an
// operator a coarse trip-wire, not an accurate invoice.
package cost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// EstimatedCostPerCallGBP is a deliberately rough, conservative estimate
// of one save_memory or search_memory call's AgentCore cost (Runtime +
// Memory + Observability, per PROPOSAL.md's cost-governance section). It
// is not derived from real billing data and is not per-backend: the
// memory backend costs nothing, but Guard has no way to know which
// backend is bound, so every call counts identically.
const EstimatedCostPerCallGBP = 0.002

// DefaultSummaryEvery is how often (in calls) Guard emits a cost_summary
// NDJSON event.
const DefaultSummaryEvery = 20

// ErrBudgetExceeded is returned by Allow once the rolling estimate has
// reached the configured cap. It is the per-call analogue of Chiron's
// exit-code-4 pre-spend block: a long-running server has no process to
// exit, so an over-budget request is rejected instead (docs/DECISIONS.md).
var ErrBudgetExceeded = errors.New("cost: rolling estimate has reached the configured budget cap")

// Summary is the payload of a cost_summary NDJSON event. It carries only
// counters and the estimate — never call content.
type Summary struct {
	Calls        uint64  `json:"calls"`
	EstimatedGBP float64 `json:"estimated_gbp"`
	CapGBP       float64 `json:"cap_gbp,omitempty"`
}

// event is the NDJSON envelope, mirroring Chiron's transport.Event shape
// (time, kind, payload) so a cost_summary line is recognisable by the
// same convention across the suite.
type event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Payload Summary   `json:"payload"`
}

// Guard tracks a rolling, call-count-based cost estimate against an
// optional monthly cap and rejects further calls once the estimate
// reaches it. A zero cap means uncapped. The counter is process-lifetime
// only: it is not persisted, and a restart resets it (a known v1
// simplification — see TODO.md).
type Guard struct {
	mu           sync.Mutex
	capGBP       float64
	perCallGBP   float64
	summaryEvery uint64
	calls        uint64
	w            io.Writer
}

// NewGuard returns a Guard capped at capGBP (zero for uncapped) that
// writes periodic cost_summary NDJSON lines to w. A nil w defaults to
// os.Stderr, matching the suite's convention of events on stderr and
// results on stdout.
func NewGuard(capGBP float64, w io.Writer) *Guard {
	if w == nil {
		w = os.Stderr
	}
	return &Guard{
		capGBP:       capGBP,
		perCallGBP:   EstimatedCostPerCallGBP,
		summaryEvery: DefaultSummaryEvery,
		w:            w,
	}
}

// Allow reports whether another call may proceed. It returns
// ErrBudgetExceeded without counting the call if the estimate already
// reached the cap; otherwise it counts the call and, every
// DefaultSummaryEvery calls, emits a cost_summary line.
func (g *Guard) Allow() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.capGBP > 0 && g.estimateLocked() >= g.capGBP {
		return fmt.Errorf("%w (estimated %.4f GBP over %d calls, cap %.2f GBP)", ErrBudgetExceeded, g.estimateLocked(), g.calls, g.capGBP)
	}

	g.calls++
	if g.summaryEvery > 0 && g.calls%g.summaryEvery == 0 {
		g.emitSummaryLocked()
	}
	return nil
}

// Summary returns the current rolling estimate.
func (g *Guard) Summary() Summary {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.summaryLocked()
}

func (g *Guard) estimateLocked() float64 {
	return float64(g.calls) * g.perCallGBP
}

func (g *Guard) summaryLocked() Summary {
	return Summary{
		Calls:        g.calls,
		EstimatedGBP: g.estimateLocked(),
		CapGBP:       g.capGBP,
	}
}

// emitSummaryLocked writes one cost_summary NDJSON line. Marshal errors
// are not possible for this fixed, content-free shape, so the write
// error is the only failure mode, and it is deliberately swallowed: a
// blocked or broken stderr must not take down the MCP server.
func (g *Guard) emitSummaryLocked() {
	line, err := json.Marshal(event{
		Time:    time.Now().UTC(),
		Kind:    "cost_summary",
		Payload: g.summaryLocked(),
	})
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = g.w.Write(line)
}
