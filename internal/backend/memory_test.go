package backend

import (
	"context"
	"testing"
)

func TestMemorySaveThenSearchFinds(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	id, err := m.Save(ctx, SaveRequest{Content: "the horse likes carrots", Kind: KindFact})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id == "" {
		t.Fatal("Save returned an empty memory ID")
	}

	results, err := m.Search(ctx, SearchRequest{Query: "carrots"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search returned %d results, want 1", len(results))
	}
	if results[0].MemoryID != id {
		t.Errorf("MemoryID = %q, want %q", results[0].MemoryID, id)
	}
	if results[0].Content != "the horse likes carrots" {
		t.Errorf("Content = %q, want original content", results[0].Content)
	}
	if results[0].CreatedAt == "" {
		t.Error("CreatedAt is empty")
	}
	if results[0].Score <= 0 {
		t.Errorf("Score = %v, want > 0 for a matching record", results[0].Score)
	}
}

func TestMemorySearchRespectsLimit(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	for i := 0; i < 10; i++ {
		if _, err := m.Save(ctx, SaveRequest{Content: "hay for the horse"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	results, err := m.Search(ctx, SearchRequest{Query: "hay", Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Search returned %d results, want 3", len(results))
	}
}

func TestMemorySearchDefaultLimit(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	for i := 0; i < 10; i++ {
		if _, err := m.Save(ctx, SaveRequest{Content: "hay for the horse"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	// Limit unset (zero value): the backend does not have to enforce the
	// documented default of 5 itself (that's the MCP tool layer's job),
	// but it must not panic or behave unboundedly strangely; document
	// current behaviour: an unset limit falls back to 5 here too.
	results, err := m.Search(ctx, SearchRequest{Query: "hay"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("Search with unset limit returned %d results, want 5", len(results))
	}
}

func TestMemorySearchEmptyStore(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	results, err := m.Search(ctx, SearchRequest{Query: "anything"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search on empty store returned %d results, want 0", len(results))
	}
}

func TestMemorySearchRanksBestMatchFirst(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	if _, err := m.Save(ctx, SaveRequest{Content: "unrelated content about tack"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantID, err := m.Save(ctx, SaveRequest{Content: "the paddock gate was left open"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	results, err := m.Search(ctx, SearchRequest{Query: "paddock gate", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].MemoryID != wantID {
		t.Fatalf("Search top result = %+v, want memory %q first", results, wantID)
	}
}

func TestMemorySearchNoMatchesStillReturnsRecords(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryBackend()

	if _, err := m.Save(ctx, SaveRequest{Content: "hello world"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A naive substring backend has no obligation to filter out
	// non-matches; it ranks everything and returns up to limit.
	results, err := m.Search(ctx, SearchRequest{Query: "completely unrelated query"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search returned %d results, want 1", len(results))
	}
	if results[0].Score != 0 {
		t.Errorf("Score = %v, want 0 for no token overlap", results[0].Score)
	}
}
