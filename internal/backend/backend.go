// Package backend defines the Backend seam: where Billet actually stores
// and recalls memory. Namespace and session identity are bound into a
// concrete Backend at construction time from BilletConfig — not carried
// on SaveRequest/SearchRequest — so the calling LLM can never select which
// tenant's memory it reads or writes (docs/DECISIONS.md, "no namespace
// parameter on the tools").
//
// v1 ships three implementations: Memory (in-process, ephemeral, the
// default), Bolt (a local, persistent bbolt-backed store), and the AWS
// Bedrock AgentCore Memory adapter. Selecting between them is a config
// change, not a code change.
package backend

import "context"

// Kind hints how a saved memory should be treated. Backends may ignore it.
type Kind string

const (
	KindEvent Kind = "event"
	KindFact  Kind = "fact"
)

// SaveRequest is one memory to persist.
type SaveRequest struct {
	// Content is the raw turn content or an explicit fact.
	Content string
	// Kind optionally hints how the backend should treat Content.
	// Empty defaults to KindEvent.
	Kind Kind
}

// SearchRequest asks for memories related to Query.
type SearchRequest struct {
	Query string
	// Limit caps the number of returned records. Callers should apply
	// the documented default (5) before calling Search; a Backend is not
	// required to apply one itself.
	Limit int
}

// Record is one memory returned from Search.
type Record struct {
	MemoryID  string
	Content   string
	Score     float64
	CreatedAt string // RFC 3339
}

// Backend saves and searches memory for the single namespace it was
// constructed with. Implementations must be safe for concurrent use.
type Backend interface {
	// Save persists req and returns an opaque, backend-assigned memory
	// ID. Acceptance does not imply the memory is immediately
	// searchable — a backend may extract or index asynchronously.
	Save(ctx context.Context, req SaveRequest) (memoryID string, err error)

	// Search returns memories related to req.Query, best matches first.
	Search(ctx context.Context, req SearchRequest) ([]Record, error)
}
