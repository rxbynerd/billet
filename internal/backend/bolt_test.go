package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

const testNamespace = "test-namespace"

func TestBoltPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b1, err := NewBoltBackend(path, testNamespace)
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

	b2, err := NewBoltBackend(path, testNamespace)
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

// TestBoltNamespaceIsolation proves namespace binds to a bucket, not
// just a file: two namespaces sharing one database file never see each
// other's memories.
func TestBoltNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	a1, err := NewBoltBackend(path, "namespace-a")
	if err != nil {
		t.Fatalf("NewBoltBackend(a): %v", err)
	}
	id, err := a1.Save(ctx, SaveRequest{Content: "tenant a's secret memory"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := NewBoltBackend(path, "namespace-b")
	if err != nil {
		t.Fatalf("NewBoltBackend(b): %v", err)
	}
	results, err := b.Search(ctx, SearchRequest{Query: "secret"})
	if err != nil {
		t.Fatalf("Search(b): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search under namespace-b returned %d results, want 0 (namespace-a's memory leaked)", len(results))
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	a2, err := NewBoltBackend(path, "namespace-a")
	if err != nil {
		t.Fatalf("reopen NewBoltBackend(a): %v", err)
	}
	defer a2.Close()
	results, err = a2.Search(ctx, SearchRequest{Query: "secret"})
	if err != nil {
		t.Fatalf("Search(a) after reopen: %v", err)
	}
	if len(results) != 1 || results[0].MemoryID != id {
		t.Fatalf("Search under namespace-a after reopen = %+v, want the original memory %q", results, id)
	}
}

func TestBoltFailsClosedOnMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "billet.db")

	if _, err := NewBoltBackend(path, testNamespace); err == nil {
		t.Fatal("NewBoltBackend succeeded despite a missing parent directory")
	}
}

func TestBoltFailsClosedOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")
	if err := os.WriteFile(path, []byte("not a bbolt database"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := NewBoltBackend(path, testNamespace); err == nil {
		t.Fatal("NewBoltBackend succeeded despite a corrupt database file")
	}
}

func TestBoltFailsClosedOnLockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")

	holder, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer holder.Close()

	_, err = NewBoltBackend(path, testNamespace)
	if err == nil {
		t.Fatal("NewBoltBackend succeeded despite the database already being open")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error = %q, want it to mention the database is locked", err.Error())
	}
}

// TestBoltFailsClosedOnLoosePermsPreExistingFile pins the deliberate
// divergence from internal/secret's warn-only precedent for a
// pre-existing file with permissions looser than 0600 (docs/DECISIONS.md):
// bolt refuses to open it rather than silently reusing it.
func TestBoltFailsClosedOnLoosePermsPreExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // deliberately open permissions: this test exercises the rejection they trigger
		t.Fatalf("WriteFile: %v", err)
	}

	b, err := NewBoltBackend(path, testNamespace)
	if err == nil {
		t.Fatal("NewBoltBackend succeeded despite pre-existing loose file permissions")
	}
	if b != nil {
		t.Errorf("NewBoltBackend returned a non-nil backend alongside an error: %+v", b)
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error = %q, want it to mention permissions", err.Error())
	}

	// Fixing the permissions and reopening must not fail with a lock
	// error, proving the rejected attempt above did not leak a handle.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	reopened, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend after fixing permissions: %v", err)
	}
	defer reopened.Close()
}

func TestBoltDatabaseFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path, testNamespace)
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

	b, err := NewBoltBackend(path, testNamespace)
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
		bucket := tx.Bucket(b.bucket)
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

// TestBoltSearchSkipsCorruptRecords pins the skip-and-count corruption
// policy: a record that fails to decode does not abort the whole
// search, it is skipped, counted, and logged once.
func TestBoltSearchSkipsCorruptRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	wantID, err := b.Save(ctx, SaveRequest{Content: "the paddock gate was left open"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(b.bucket)
		if err := bucket.Put(seqKey(9001), []byte("not valid json")); err != nil {
			return err
		}
		return bucket.Put([]byte("bad"), []byte("also not valid json"))
	})
	if err != nil {
		t.Fatalf("Update (inject corrupt records): %v", err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	results, err := b.Search(ctx, SearchRequest{Query: "paddock"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].MemoryID != wantID {
		t.Fatalf("Search = %+v, want only the one valid record %q", results, wantID)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "skipped undecodable records") {
		t.Errorf("log output = %q, want a skipped-records warning", logged)
	}
	if !strings.Contains(logged, "count=2") {
		t.Errorf("log output = %q, want count=2", logged)
	}
}

// TestBoltSaveMissingBucketReturnsError and
// TestBoltSearchMissingBucketReturnsError pin fail-closed behaviour when
// the namespace bucket has been removed out from under an open Bolt
// (bucket corruption, manual bbolt surgery): both methods must return an
// error, not panic or silently report "no memories."
func TestBoltSaveMissingBucketReturnsError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	if err := b.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket(b.bucket)
	}); err != nil {
		t.Fatalf("Update (delete bucket): %v", err)
	}

	_, err = b.Save(ctx, SaveRequest{Content: "anything"})
	if err == nil {
		t.Fatal("Save succeeded despite a missing bucket")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error = %q, want it to mention the bucket is missing", err.Error())
	}
}

func TestBoltSearchMissingBucketReturnsError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "billet.db")

	b, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	if err := b.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket(b.bucket)
	}); err != nil {
		t.Fatalf("Update (delete bucket): %v", err)
	}

	_, err = b.Search(ctx, SearchRequest{Query: "anything"})
	if err == nil {
		t.Fatal("Search succeeded despite a missing bucket")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error = %q, want it to mention the bucket is missing", err.Error())
	}
}

func TestBoltSaveRejectsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")
	b, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = b.Save(ctx, SaveRequest{Content: "anything"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want it to wrap context.Canceled", err)
	}
}

func TestBoltSearchRejectsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.db")
	b, err := NewBoltBackend(path, testNamespace)
	if err != nil {
		t.Fatalf("NewBoltBackend: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = b.Search(ctx, SearchRequest{Query: "anything"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search error = %v, want it to wrap context.Canceled", err)
	}
}

