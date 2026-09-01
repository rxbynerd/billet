package backend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// recordsBucket holds every saved memory, keyed by an 8-byte big-endian
// sequence number so iteration order matches insertion order.
var recordsBucket = []byte("records")

// Bolt is a persistent, file-backed Backend using a single bbolt
// database. It is the durable, dependency-free alternative to
// agentcore-memory: no AWS account required, but memories survive
// process restarts unlike Memory.
type Bolt struct {
	db *bbolt.DB
}

var _ Backend = (*Bolt)(nil)
var _ io.Closer = (*Bolt)(nil)

// NewBoltBackend opens (or creates) the bbolt database at path and
// returns a ready Bolt. It does not create path's parent directory: a
// missing directory fails construction rather than silently creating
// unexpected paths on disk. Construction also runs a write probe that
// pre-creates recordsBucket, so a permissions problem is caught at
// startup rather than on the first save_memory call.
func NewBoltBackend(path string) (*Bolt, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("bolt: open %s: database is locked (is another billet serve using it?)", path)
		}
		return nil, fmt.Errorf("bolt: open %s: %w", path, err)
	}

	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(recordsBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("bolt: initialize %s: %w", path, err)
	}

	return &Bolt{db: db}, nil
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
// locking.
func (b *Bolt) Save(_ context.Context, req SaveRequest) (string, error) {
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
		bucket := tx.Bucket(recordsBucket)
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
// results.
func (b *Bolt) Search(_ context.Context, req SearchRequest) ([]Record, error) {
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

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(recordsBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var rec boltRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
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
