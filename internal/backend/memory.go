package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is the v1 default Backend: an in-process, non-persistent store
// safe to call unconditionally with no external dependencies and no
// network — the backend used when nothing is configured, and the one
// exercised by the test suite. Every record is lost on process exit.
//
// Search uses trivial case-insensitive token-overlap scoring, not
// semantic recall: it is a placeholder for local development and tests,
// not a substitute for the agentcore-memory backend's extraction and
// retrieval.
type Memory struct {
	mu      sync.Mutex
	records []storedRecord
}

type storedRecord struct {
	Record
	createdAt time.Time
}

var _ Backend = (*Memory)(nil)

// NewMemoryBackend returns an empty Memory backend.
func NewMemoryBackend() *Memory {
	return &Memory{}
}

// Save appends req to the in-process store and returns a newly generated
// memory ID. Never fails except on an unreadable random source.
func (m *Memory) Save(_ context.Context, req SaveRequest) (string, error) {
	id, err := randomHexID("mem")
	if err != nil {
		return "", fmt.Errorf("memory: generate id: %w", err)
	}

	now := time.Now().UTC()
	m.mu.Lock()
	m.records = append(m.records, storedRecord{
		Record: Record{
			MemoryID:  id,
			Content:   req.Content,
			CreatedAt: now.Format(time.RFC3339),
		},
		createdAt: now,
	})
	m.mu.Unlock()

	return id, nil
}

// Search scores every stored record against req.Query by case-insensitive
// token overlap and returns up to req.Limit results, best score first,
// most recent first among ties. An empty store or a limit <= 0 with no
// override returns no results only when there is nothing stored.
func (m *Memory) Search(_ context.Context, req SearchRequest) ([]Record, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}

	m.mu.Lock()
	candidates := make([]storedRecord, len(m.records))
	copy(candidates, m.records)
	m.mu.Unlock()

	if len(candidates) == 0 {
		return nil, nil
	}

	queryTokens := tokenize(req.Query)

	type scored struct {
		storedRecord
		score float64
	}
	results := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		results = append(results, scored{storedRecord: c, score: overlapScore(queryTokens, c.Content)})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].createdAt.After(results[j].createdAt)
	})

	if len(results) > limit {
		results = results[:limit]
	}

	out := make([]Record, len(results))
	for i, r := range results {
		rec := r.Record
		rec.Score = r.score
		out[i] = rec
	}
	return out, nil
}

// tokenize lower-cases and splits on whitespace; an empty query yields no
// tokens, so every record scores 0 and ranking falls back to recency.
func tokenize(s string) []string {
	fields := strings.Fields(strings.ToLower(s))
	return fields
}

// overlapScore is the fraction of queryTokens that appear as a substring
// of content (case-insensitive). Zero query tokens always score 0.
func overlapScore(queryTokens []string, content string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	lower := strings.ToLower(content)
	matches := 0
	for _, tok := range queryTokens {
		if strings.Contains(lower, tok) {
			matches++
		}
	}
	return float64(matches) / float64(len(queryTokens))
}
