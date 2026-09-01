package backend

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testBackendContract exercises the behaviour every Backend
// implementation must satisfy, independent of storage mechanism.
// newBackend must return a ready, empty Backend; the test owns cleanup
// via t.Cleanup.
func testBackendContract(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Run("SaveThenSearchFinds", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		id, err := b.Save(ctx, SaveRequest{Content: "the horse likes carrots", Kind: KindFact})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if id == "" {
			t.Fatal("Save returned an empty memory ID")
		}

		results, err := b.Search(ctx, SearchRequest{Query: "carrots"})
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
	})

	t.Run("SaveReturnsUniqueIDs", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		seen := make(map[string]bool)
		for i := 0; i < 5; i++ {
			id, err := b.Save(ctx, SaveRequest{Content: fmt.Sprintf("memory %d", i)})
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if id == "" {
				t.Fatal("Save returned an empty memory ID")
			}
			if seen[id] {
				t.Fatalf("Save returned duplicate ID %q", id)
			}
			seen[id] = true
		}
	})

	t.Run("SearchRanksBestMatchFirst", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		if _, err := b.Save(ctx, SaveRequest{Content: "unrelated content about tack"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		wantID, err := b.Save(ctx, SaveRequest{Content: "the paddock gate was left open"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}

		results, err := b.Search(ctx, SearchRequest{Query: "paddock gate", Limit: 2})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) == 0 || results[0].MemoryID != wantID {
			t.Fatalf("Search top result = %+v, want memory %q first", results, wantID)
		}
	})

	t.Run("SearchTiebreaksNewestFirst", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		if _, err := b.Save(ctx, SaveRequest{Content: "the paddock gate was left open"}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		wantID, err := b.Save(ctx, SaveRequest{Content: "the paddock gate was left open"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}

		results, err := b.Search(ctx, SearchRequest{Query: "paddock gate", Limit: 1})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 1 || results[0].MemoryID != wantID {
			t.Fatalf("Search top result = %+v, want the most recently saved memory %q", results, wantID)
		}
	})

	t.Run("SearchRespectsLimit", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		for i := 0; i < 10; i++ {
			if _, err := b.Save(ctx, SaveRequest{Content: "hay for the horse"}); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}

		results, err := b.Search(ctx, SearchRequest{Query: "hay", Limit: 3})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("Search returned %d results, want 3", len(results))
		}
	})

	t.Run("SearchDefaultLimit", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		for i := 0; i < 7; i++ {
			if _, err := b.Save(ctx, SaveRequest{Content: "hay for the horse"}); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}

		results, err := b.Search(ctx, SearchRequest{Query: "hay"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 5 {
			t.Fatalf("Search with unset limit returned %d results, want 5", len(results))
		}
	})

	t.Run("SearchClampsExcessiveLimit", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		for i := 0; i < maxSearchResults+10; i++ {
			if _, err := b.Save(ctx, SaveRequest{Content: "hay for the horse"}); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}

		results, err := b.Search(ctx, SearchRequest{Query: "hay", Limit: 1 << 31})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != maxSearchResults {
			t.Fatalf("Search with Limit 1<<31 returned %d results, want the %d ceiling", len(results), maxSearchResults)
		}
	})

	t.Run("SearchNoMatchesStillReturnsRecords", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		if _, err := b.Save(ctx, SaveRequest{Content: "hello world"}); err != nil {
			t.Fatalf("Save: %v", err)
		}

		results, err := b.Search(ctx, SearchRequest{Query: "completely unrelated query"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("Search returned %d results, want 1", len(results))
		}
		if results[0].Score != 0 {
			t.Errorf("Score = %v, want 0 for no token overlap", results[0].Score)
		}
	})

	t.Run("SearchEmptyStore", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		results, err := b.Search(ctx, SearchRequest{Query: "anything"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("Search on empty store returned %d results, want 0", len(results))
		}
	})

	t.Run("ConcurrentSaveAndSearch", func(t *testing.T) {
		ctx := context.Background()
		b := newBackend(t)

		const goroutines = 50
		var wg sync.WaitGroup
		wg.Add(goroutines * 2)
		for i := 0; i < goroutines; i++ {
			go func(i int) {
				defer wg.Done()
				if _, err := b.Save(ctx, SaveRequest{Content: fmt.Sprintf("concurrent memory %d", i)}); err != nil {
					t.Errorf("Save: %v", err)
				}
			}(i)
			go func() {
				defer wg.Done()
				if _, err := b.Search(ctx, SearchRequest{Query: "concurrent"}); err != nil {
					t.Errorf("Search: %v", err)
				}
			}()
		}
		wg.Wait()

		results, err := b.Search(ctx, SearchRequest{Query: "concurrent", Limit: goroutines})
		if err != nil {
			t.Fatalf("final Search: %v", err)
		}
		if len(results) != goroutines {
			t.Fatalf("final Search returned %d records, want all %d saved records", len(results), goroutines)
		}
	})
}

func TestBackendContract(t *testing.T) {
	tests := []struct {
		name       string
		newBackend func(t *testing.T) Backend
	}{
		{
			name: "Memory",
			newBackend: func(t *testing.T) Backend {
				return NewMemoryBackend()
			},
		},
		{
			name: "Bolt",
			newBackend: func(t *testing.T) Backend {
				b, err := NewBoltBackend(filepath.Join(t.TempDir(), "billet.db"), "test-namespace")
				if err != nil {
					t.Fatalf("NewBoltBackend: %v", err)
				}
				t.Cleanup(func() {
					if err := b.Close(); err != nil {
						t.Errorf("Close: %v", err)
					}
				})
				return b
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testBackendContract(t, tt.newBackend)
		})
	}
}
