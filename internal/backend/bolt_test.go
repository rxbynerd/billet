package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func TestBoltPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b1, err := NewBoltBackend(path)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	id, err := b1.Save(ctx, SaveRequest{Content: "the paddock gate was left open"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := b1.Search(ctx, SearchRequest{Query: "paddock"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("Search returned %d results, want 1", len(before))
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b2, err := NewBoltBackend(path)
	if err != nil {
		t.Fatalf("reopen NewBoltBackend: %v", err)
	}
	defer b2.Close()

	after, err := b2.Search(ctx, SearchRequest{Query: "paddock"})
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("Search after reopen returned %d results, want 1", len(after))
	}
	if after[0].MemoryID != id {
		t.Errorf("MemoryID after reopen = %q, want %q", after[0].MemoryID, id)
	}
	if after[0].CreatedAt != before[0].CreatedAt {
		t.Errorf("CreatedAt after reopen = %q, want %q", after[0].CreatedAt, before[0].CreatedAt)
	}

	newID, err := b2.Save(ctx, SaveRequest{Content: "another memory after reopen"})
	if err != nil {
		t.Fatalf("Save after reopen: %v", err)
	}
	if newID == id {
		t.Fatal("Save after reopen returned the same memory ID as before")
	}

	results, err := b2.Search(ctx, SearchRequest{Query: "paddock reopen", Limit: 5})
	if err != nil {
		t.Fatalf("Search after second save: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search after second save returned %d results, want 2 (no sequence collision)", len(results))
	}
}

func TestBoltFailsClosedOnMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "billet.db")

	if _, err := NewBoltBackend(path); err == nil {
		t.Fatal("NewBoltBackend succeeded despite a missing parent directory")
	}
}

func TestBoltFailsClosedOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")
	if err := os.WriteFile(path, []byte("not a bbolt database"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := NewBoltBackend(path); err == nil {
		t.Fatal("NewBoltBackend succeeded despite a corrupt database file")
	}
}

func TestBoltFailsClosedOnLockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")

	holder, err := NewBoltBackend(path)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer holder.Close()

	_, err = NewBoltBackend(path)
	if err == nil {
		t.Fatal("NewBoltBackend succeeded despite the database already being open")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error = %q, want it to mention the database is locked", err.Error())
	}
}

func TestBoltDatabaseFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}
}

func TestBoltPersistsKind(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	id, err := b.Save(ctx, SaveRequest{Content: "the horse likes carrots", Kind: KindFact})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	var found *boltRecord
	err = b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(recordsBucket)
		return bucket.ForEach(func(k, v []byte) error {
			var rec boltRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.MemoryID == id {
				found = &rec
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if found == nil {
		t.Fatal("stored record not found via raw bbolt View")
	}
	if found.Kind != string(KindFact) {
		t.Errorf("Kind = %q, want %q", found.Kind, KindFact)
	}
}
