package cost

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAllowUncappedNeverBlocks(t *testing.T) {
	g := NewGuard(0, &bytes.Buffer{})
	for i := 0; i < 1000; i++ {
		if err := g.Allow(); err != nil {
			t.Fatalf("Allow() call %d = %v, want nil (uncapped)", i, err)
		}
	}
}

func TestAllowBlocksOnceCapReached(t *testing.T) {
	// A cap equal to exactly one call's estimate allows the first call
	// (estimate is 0 before it runs) and blocks the second (estimate
	// after the first call has already reached the cap).
	budgetCap := EstimatedCostPerCallGBP
	g := NewGuard(budgetCap, &bytes.Buffer{})

	if err := g.Allow(); err != nil {
		t.Fatalf("first Allow() = %v, want nil", err)
	}
	err := g.Allow()
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second Allow() = %v, want ErrBudgetExceeded", err)
	}
}

func TestAllowErrorNeverIncrementsCount(t *testing.T) {
	budgetCap := EstimatedCostPerCallGBP
	g := NewGuard(budgetCap, &bytes.Buffer{})

	if err := g.Allow(); err != nil {
		t.Fatalf("first Allow() = %v, want nil", err)
	}
	for i := 0; i < 5; i++ {
		if err := g.Allow(); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("Allow() call %d = %v, want ErrBudgetExceeded", i, err)
		}
	}
	if got := g.Summary().Calls; got != 1 {
		t.Errorf("Summary().Calls = %d, want 1 (blocked calls must not count)", got)
	}
}

// TestGuardAllowRacesTheLimit drives Allow from many goroutines at once,
// right at the budget cap boundary (run this test with -race). It proves
// the check-then-increment in Allow is atomic under real contention: the
// number of accepted calls must never exceed what the budget allows, not
// just by code inspection of the mutex.
func TestGuardAllowRacesTheLimit(t *testing.T) {
	const allowedCalls = 20
	budgetCap := float64(allowedCalls) * EstimatedCostPerCallGBP
	g := NewGuard(budgetCap, &bytes.Buffer{})

	const goroutines = 200
	var accepted atomic.Uint64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := g.Allow(); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got > allowedCalls {
		t.Errorf("accepted %d calls concurrently, want at most %d for a %.4f GBP cap", got, allowedCalls, budgetCap)
	}
	if got := g.Summary().Calls; got != accepted.Load() {
		t.Errorf("Summary().Calls = %d, want it to match the %d accepted calls", got, accepted.Load())
	}
}

func TestSummaryReflectsCallsAndEstimate(t *testing.T) {
	g := NewGuard(0, &bytes.Buffer{})
	for i := 0; i < 3; i++ {
		if err := g.Allow(); err != nil {
			t.Fatalf("Allow(): %v", err)
		}
	}
	s := g.Summary()
	if s.Calls != 3 {
		t.Errorf("Calls = %d, want 3", s.Calls)
	}
	want := 3 * EstimatedCostPerCallGBP
	if s.EstimatedGBP != want {
		t.Errorf("EstimatedGBP = %v, want %v", s.EstimatedGBP, want)
	}
}

func TestEmitsCostSummaryEveryNCalls(t *testing.T) {
	var buf bytes.Buffer
	g := NewGuard(0, &buf)
	g.summaryEvery = 4

	for i := 0; i < 9; i++ {
		if err := g.Allow(); err != nil {
			t.Fatalf("Allow(): %v", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("emitted %d NDJSON lines for 9 calls at every-4, want 2", len(lines))
	}
	for _, line := range lines {
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not valid JSON: %v (%q)", err, line)
		}
		if ev.Kind != "cost_summary" {
			t.Errorf("Kind = %q, want cost_summary", ev.Kind)
		}
		if ev.Time.IsZero() {
			t.Error("Time is zero")
		}
	}
}

func TestCostSummaryNeverIncludesContent(t *testing.T) {
	var buf bytes.Buffer
	g := NewGuard(0, &buf)
	g.summaryEvery = 1

	if err := g.Allow(); err != nil {
		t.Fatalf("Allow(): %v", err)
	}

	// The Summary/event types structurally carry only counters, but pin
	// the wire shape too: a call's content never reaches this package,
	// so it cannot leak into the emitted line.
	if strings.Contains(buf.String(), "content") {
		t.Errorf("cost_summary line unexpectedly mentions content: %q", buf.String())
	}
	var ev event
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("emitted line is not valid JSON: %v", err)
	}
}
