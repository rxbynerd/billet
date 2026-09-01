package backend

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// recordsBucketName returns the per-namespace bucket holding every memory
// saved under namespace, keyed by an 8-byte big-endian sequence number so
// iteration order matches insertion order. One database file may be
// shared by several namespaces without their memories mixing, since each
// namespace gets its own bucket.
func recordsBucketName(namespace string) []byte {
	return []byte("records/" + namespace)
}

// Bolt is a persistent, file-backed Backend using a single bbolt
// database. It is the durable, dependency-free alternative to
// agentcore-memory: no AWS account required, but memories survive
// process restarts unlike Memory.
type Bolt struct {
	db     *bbolt.DB
	bucket []byte
}

var _ Backend = (*Bolt)(nil)
var _ io.Closer = (*Bolt)(nil)

// NewBoltBackend opens (or creates) the bbolt database at path, bound to
// namespace, and returns a ready Bolt. It does not create path's parent
// directory: a missing directory fails construction rather than
// silently creating unexpected paths on disk. Construction also runs a
// write probe that pre-creates namespace's bucket, so a permissions
// problem is caught at startup rather than on the first save_memory
// call.
//
// A pre-existing file at path with permissions looser than 0600 fails
// construction rather than being silently reused: unlike a freshly
// created file, bbolt.Open does not tighten an existing file's mode, and
// this backend stores memory content in plaintext.
func NewBoltBackend(path, namespace string) (*Bolt, error) {
	preexisted, err := preexistingLoosePermissions(path)
	if err != nil {
		return nil, err
	}
	if preexisted {
		return nil, fmt.Errorf("bolt: database file %s has permissions looser than 0600; run chmod 600 %s", path, path)
	}

	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("bolt: open %s: database is locked (is another billet serve using it?)", path)
		}
		return nil, fmt.Errorf("bolt: open %s: %w", path, err)
	}

	bucket := recordsBucketName(namespace)
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		err = fmt.Errorf("bolt: initialize %s: %w", path, err)
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bolt: close %s: %w", path, closeErr))
		}
		return nil, err
	}

	return &Bolt{db: db, bucket: bucket}, nil
}

// preexistingLoosePermissions reports whether path already names a file
// with permission bits beyond owner read/write. A nonexistent path (the
// common case: bbolt will create it) is not an error here.
func preexistingLoosePermissions(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("bolt: stat %s: %w", path, err)
	}
	return info.Mode().Perm()&0o077 != 0, nil
}

// boltRecord is the JSON encoding stored as each record's bbolt value.
type boltRecord struct {
	MemoryID  string `json:"memoryId"`
	Content   string `json:"content"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"createdAt"`
}

// Save persists req in a single write transaction, keyed by the
// bucket's next sequence number, and returns a newly generated memory
// ID. bbolt.DB is safe for concurrent use, so Save needs no additional
// locking. ctx is checked before entering the transaction, but bbolt has
// no cancellable transaction API: an already-in-flight write still runs
// to completion even if ctx is canceled while it is queued or running.
func (b *Bolt) Save(ctx context.Context, req SaveRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("bolt: save: %w", err)
	}

	id, err := randomHexID("mem")
	if err != nil {
		return "", fmt.Errorf("bolt: generate id: %w", err)
	}

	kind := req.Kind
	if kind == "" {
		kind = KindEvent
	}

	rec := boltRecord{
		MemoryID:  id,
		Content:   req.Content,
		Kind:      string(kind),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(b.bucket)
		if bucket == nil {
			return fmt.Errorf("bolt: bucket %q missing", b.bucket)
		}
		seq, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		value, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put(seqKey(seq), value)
	})
	if err != nil {
		return "", fmt.Errorf("bolt: save: %w", err)
	}

	return id, nil
}

// Search scores every stored record against req.Query by the same
// case-insensitive token-overlap heuristic Memory uses (tokenize,
// overlapScore) and returns up to req.Limit results, best score first,
// most recently inserted first among ties. An empty database returns no
// results. A record that fails to decode (a torn write, or a schema from
// an incompatible future version) is skipped and counted rather than
// aborting the whole search; skipped records are logged once per call.
// ctx is checked before entering the transaction; see Save's doc comment
// on bbolt's lack of transaction cancellation.
func (b *Bolt) Search(ctx context.Context, req SearchRequest) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("bolt: search: %w", err)
	}

	limit := req.Limit
	switch {
	case limit <= 0:
		limit = 5
	case limit > maxSearchResults:
		limit = maxSearchResults
	}

	queryTokens := tokenize(req.Query)

	type scored struct {
		Record
		seq   uint64
		score float64
	}
	var candidates []scored
	var skipped int
	var firstBadKey []byte

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(b.bucket)
		if bucket == nil {
			return fmt.Errorf("bolt: bucket %q missing", b.bucket)
		}
		return bucket.ForEach(func(k, v []byte) error {
			if v == nil || len(k) != 8 {
				skipped++
				if firstBadKey == nil {
					firstBadKey = append([]byte(nil), k...)
				}
				return nil
			}
			var rec boltRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				skipped++
				if firstBadKey == nil {
					firstBadKey = append([]byte(nil), k...)
				}
				return nil
			}
			candidates = append(candidates, scored{
				Record: Record{
					MemoryID:  rec.MemoryID,
					Content:   rec.Content,
					CreatedAt: rec.CreatedAt,
				},
				seq:   binary.BigEndian.Uint64(k),
				score: overlapScore(queryTokens, rec.Content),
			})
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("bolt: search: %w", err)
	}
	if skipped > 0 {
		slog.Warn("bolt: skipped undecodable records", "count", skipped, "first_bad_key", hex.EncodeToString(firstBadKey))
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].seq > candidates[j].seq
	})

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	out := make([]Record, len(candidates))
	for i, c := range candidates {
		rec := c.Record
		rec.Score = c.score
		out[i] = rec
	}
	return out, nil
}

// Close releases the underlying database file lock.
func (b *Bolt) Close() error {
	return b.db.Close()
}

// seqKey encodes seq as an 8-byte big-endian bbolt key, so bucket
// iteration order matches insertion order.
func seqKey(seq uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	return key
}
