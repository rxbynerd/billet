package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
)

type erroringBackend struct {
	err error
}

func (e erroringBackend) Save(context.Context, backend.SaveRequest) (string, error) {
	return "", e.err
}

func (e erroringBackend) Search(context.Context, backend.SearchRequest) ([]backend.Record, error) {
	return nil, e.err
}

type recordingBackend struct {
	mu       sync.Mutex
	lastSave backend.SaveRequest
	lastSrch backend.SearchRequest
}

func (r *recordingBackend) Save(_ context.Context, req backend.SaveRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSave = req
	return "rec-id", nil
}

func (r *recordingBackend) Search(_ context.Context, req backend.SearchRequest) ([]backend.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSrch = req
	return nil, nil
}

func uncapped() *cost.Guard { return cost.NewGuard(0, &bytes.Buffer{}) }

// TestValidationErrorsAreInvalidInput protects the classification both
// transports use to map caller-caused errors to their protocol's
// invalid-argument shape.
func TestValidationErrorsAreInvalidInput(t *testing.T) {
	svc := New(backend.NewMemoryBackend(), uncapped())
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"empty content", func() error {
			_, err := svc.SaveMemory(ctx, "  ", "")
			return err
		}},
		{"oversized content", func() error {
			_, err := svc.SaveMemory(ctx, strings.Repeat("a", MaxContentBytes+1), "")
			return err
		}},
		{"invalid kind", func() error {
			_, err := svc.SaveMemory(ctx, "x", "opinion")
			return err
		}},
		{"empty query", func() error {
			_, err := svc.SearchMemory(ctx, " ", 0)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var inv *InvalidInputError
			if !errors.As(err, &inv) {
				t.Fatalf("error = %v (%T), want *InvalidInputError", err, err)
			}
		})
	}
}

// TestBackendFailuresReturnGenericSentinels protects the error policy:
// a failing backend surfaces only the generic sentinel, never the
// underlying error detail.
func TestBackendFailuresReturnGenericSentinels(t *testing.T) {
	svc := New(erroringBackend{err: errors.New("arn:aws:iam::123456789012:role/billet denied")}, uncapped())
	ctx := context.Background()

	if _, err := svc.SaveMemory(ctx, "x", ""); !errors.Is(err, ErrSaveBackendUnavailable) {
		t.Errorf("SaveMemory error = %v, want ErrSaveBackendUnavailable", err)
	}
	if _, err := svc.SearchMemory(ctx, "x", 0); !errors.Is(err, ErrSearchBackendUnavailable) {
		t.Errorf("SearchMemory error = %v, want ErrSearchBackendUnavailable", err)
	}
}

// TestBudgetRejectionReturnsSentinel protects the budget-exhaustion path:
// both operations return ErrBudgetExceeded, with no cap or count detail.
func TestBudgetRejectionReturnsSentinel(t *testing.T) {
	guard := cost.NewGuard(cost.EstimatedCostPerCallGBP, &bytes.Buffer{})
	svc := New(backend.NewMemoryBackend(), guard)
	ctx := context.Background()

	if _, err := svc.SaveMemory(ctx, "first", ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := svc.SaveMemory(ctx, "second", ""); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("SaveMemory over budget = %v, want ErrBudgetExceeded", err)
	}
	if _, err := svc.SearchMemory(ctx, "q", 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("SearchMemory over budget = %v, want ErrBudgetExceeded", err)
	}
}

// TestKindMappingAndLimitClamping protects the wire-to-backend
// normalisation: kind strings map to backend kinds, and limit gets the
// documented default and ceiling.
func TestKindMappingAndLimitClamping(t *testing.T) {
	rec := &recordingBackend{}
	svc := New(rec, uncapped())
	ctx := context.Background()

	if _, err := svc.SaveMemory(ctx, "x", "fact"); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if rec.lastSave.Kind != backend.KindFact {
		t.Errorf("kind forwarded = %q, want %q", rec.lastSave.Kind, backend.KindFact)
	}
	if _, err := svc.SaveMemory(ctx, "x", ""); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if rec.lastSave.Kind != backend.KindEvent {
		t.Errorf("empty kind forwarded = %q, want %q", rec.lastSave.Kind, backend.KindEvent)
	}

	if _, err := svc.SearchMemory(ctx, "q", 0); err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if rec.lastSrch.Limit != DefaultSearchLimit {
		t.Errorf("zero limit forwarded = %d, want default %d", rec.lastSrch.Limit, DefaultSearchLimit)
	}
	if _, err := svc.SearchMemory(ctx, "q", MaxSearchLimit+1); err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if rec.lastSrch.Limit != MaxSearchLimit {
		t.Errorf("oversized limit forwarded = %d, want ceiling %d", rec.lastSrch.Limit, MaxSearchLimit)
	}
}

// TestNilBudgetDefaultsUncapped protects New's documented nil-budget
// default.
func TestNilBudgetDefaultsUncapped(t *testing.T) {
	svc := New(backend.NewMemoryBackend(), nil)
	if _, err := svc.SaveMemory(context.Background(), "x", ""); err != nil {
		t.Fatalf("SaveMemory with nil budget: %v", err)
	}
}
