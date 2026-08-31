// Package service implements the transport-neutral core of Billet's two
// tool operations: input validation, the cost-guard gate, limit
// clamping, and the generic caller-facing error policy. Both transports
// (internal/mcpserver, internal/rpcserver) adapt this one implementation,
// so save_memory and search_memory behave identically whether a call
// arrives directly over MCP or proxied by the control plane over RPC.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
)

const (
	// DefaultSearchLimit matches Bedrock's numberOfResults convention
	// (PROPOSAL.md) and the documented search_memory default.
	DefaultSearchLimit = 5
	// MaxSearchLimit bounds search_memory's caller-supplied limit. Without
	// a ceiling, a value near or above math.MaxInt32 would silently wrap
	// to a negative TopK/MaxResults once a backend converts it to int32.
	MaxSearchLimit = 100

	// MaxContentBytes bounds save_memory's content field. This is a
	// caller-facing limit (returned as a validation error), not an
	// attempt to size the backend's actual storage limits.
	MaxContentBytes = 256 << 10 // 256 KiB
)

// ErrBudgetExceeded, ErrSaveBackendUnavailable, and
// ErrSearchBackendUnavailable are the only detail a caller ever sees for
// a budget rejection or a backend failure, on either transport. Both
// endpoints are unauthenticated by default (docs/security.md), so the
// real detail (the exact cap and call count; AWS account IDs, ARNs, and
// request IDs on a backend error) is logged server-side via log/slog
// instead of returned to the caller.
var (
	ErrBudgetExceeded           = errors.New("budget exceeded")
	ErrSaveBackendUnavailable   = errors.New("save_memory: backend unavailable")
	ErrSearchBackendUnavailable = errors.New("search_memory: backend unavailable")
)

// InvalidInputError is a caller-caused validation error: it describes the
// caller's own request, not Billet's internals, so its message is safe to
// return verbatim on any transport. Transports use errors.As to map it to
// their protocol's invalid-argument shape.
type InvalidInputError struct {
	msg string
}

func (e *InvalidInputError) Error() string { return e.msg }

func invalidInputf(format string, args ...any) error {
	return &InvalidInputError{msg: fmt.Sprintf(format, args...)}
}

// Service executes save_memory and search_memory against a Backend,
// gated by a cost.Guard. Namespace and session identity are not part of
// Service: they are already baked into the Backend it was built with
// (internal/backend's construction-time binding).
type Service struct {
	backend backend.Backend
	budget  *cost.Guard
}

// New returns a Service delegating to b and gated by budget. A nil
// budget defaults to an uncapped guard (equivalent to cost.NewGuard(0,
// nil)) rather than panicking on first use.
func New(b backend.Backend, budget *cost.Guard) *Service {
	if budget == nil {
		budget = cost.NewGuard(0, nil)
	}
	return &Service{backend: b, budget: budget}
}

// SaveResult is a successful SaveMemory outcome.
type SaveResult struct {
	MemoryID string
	Accepted bool
}

// SaveMemory validates and persists one memory. kind is the wire-level
// hint ("", "event", or "fact"; empty defaults to event).
func (s *Service) SaveMemory(ctx context.Context, content, kind string) (SaveResult, error) {
	if err := s.budget.Allow(); err != nil {
		slog.Warn("save_memory rejected by budget guard", "error", err)
		return SaveResult{}, ErrBudgetExceeded
	}

	if strings.TrimSpace(content) == "" {
		return SaveResult{}, invalidInputf("save_memory: content must not be empty")
	}
	if len(content) > MaxContentBytes {
		return SaveResult{}, invalidInputf("save_memory: content exceeds the %d byte limit", MaxContentBytes)
	}

	k := backend.KindEvent
	switch kind {
	case "", string(backend.KindEvent):
	case string(backend.KindFact):
		k = backend.KindFact
	default:
		return SaveResult{}, invalidInputf("save_memory: kind %q is not \"event\" or \"fact\"", kind)
	}

	id, err := s.backend.Save(ctx, backend.SaveRequest{Content: content, Kind: k})
	if err != nil {
		slog.Error("save_memory: backend Save failed", "error", err)
		return SaveResult{}, ErrSaveBackendUnavailable
	}
	return SaveResult{MemoryID: id, Accepted: true}, nil
}

// SearchMemory validates the query, applies the documented default and
// ceiling to limit, and returns the best-matching records.
func (s *Service) SearchMemory(ctx context.Context, query string, limit int) ([]backend.Record, error) {
	if err := s.budget.Allow(); err != nil {
		slog.Warn("search_memory rejected by budget guard", "error", err)
		return nil, ErrBudgetExceeded
	}

	if strings.TrimSpace(query) == "" {
		return nil, invalidInputf("search_memory: query must not be empty")
	}

	switch {
	case limit <= 0:
		limit = DefaultSearchLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}

	records, err := s.backend.Search(ctx, backend.SearchRequest{Query: query, Limit: limit})
	if err != nil {
		slog.Error("search_memory: backend Search failed", "error", err)
		return nil, ErrSearchBackendUnavailable
	}
	return records, nil
}
