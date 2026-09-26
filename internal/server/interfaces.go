package server

import (
	"context"

	"github.com/1morshed1/collab-crdt/internal/crdt"
)

// This file declares the two seams that let the server stay decoupled from its
// heavier dependencies. Both are optional: a Room works with nil for either, which is
// exactly how the earliest end-to-end version runs (single process, no database, no
// Redis). Later phases provide real implementations:
//
//   - Store  is implemented by internal/store (Postgres) — see docs/04.
//   - Bus    is implemented by internal/presence (Redis pub/sub) — see docs/05.
//
// Declaring the interfaces HERE (where they are consumed) rather than in the packages
// that implement them is idiomatic Go: the consumer states exactly what it needs, and
// implementations depend on the consumer, avoiding import cycles.

// Store is the persistence seam. It records every operation and periodic snapshots so
// a document survives server restarts and loads quickly (docs/04).
//
// A note on the boundary between "what's in the snapshot" and "what's newer": we use
// the database's own auto-incrementing sequence number (seq), NOT the Lamport
// timestamp, to mark that boundary. Lamport values work for ordering inserts, but a
// delete of an old character would carry a small effective timestamp and could be
// wrongly excluded from "ops since the snapshot." A monotonic DB seq sidesteps that
// entirely. Replay is idempotent (crdt.Apply dedupes), so a loose boundary that
// replays a few already-snapshotted ops is harmless.
type Store interface {
	// EnsureDocument makes sure a row exists for this document id.
	EnsureDocument(ctx context.Context, docID string) error

	// LoadSnapshot returns the most recent snapshot for a document, the DB seq it was
	// taken up to, and ok=false if the document has no snapshot yet.
	LoadSnapshot(ctx context.Context, docID string) (data []byte, upToSeq int64, ok bool, err error)

	// OpsSince returns all operations for a document with seq strictly greater than
	// afterSeq, in ascending seq order, plus the greatest seq seen (so the caller can
	// track its position). latestSeq equals afterSeq when there are no newer ops.
	OpsSince(ctx context.Context, docID string, afterSeq int64) (ops []crdt.Operation, latestSeq int64, err error)

	// AppendOp durably records a single operation and returns the seq assigned to it.
	AppendOp(ctx context.Context, docID string, op crdt.Operation) (seq int64, err error)

	// SaveSnapshot stores a full serialized CRDT state along with the seq it includes
	// up to and how many ops are baked in (for observability/compaction decisions).
	SaveSnapshot(ctx context.Context, docID string, data []byte, upToSeq int64, opCount int64) error
}

// Bus is the cross-instance fan-out seam. When multiple server pods each hold clients
// for the same document, a message produced on one pod must reach clients on the
// others. A Bus publishes a room's outbound bytes so every subscribed pod receives
// them (docs/05). With a nil Bus the server is single-instance and broadcast stays
// in-memory.
//
// The implementation is responsible for not delivering a pod's own messages back to
// it (echo suppression), so the bytes a subscriber receives are always "from someone
// else" and can be broadcast to local clients as-is.
type Bus interface {
	// Publish sends already-encoded client-message bytes to all OTHER subscribers of a
	// document's channel.
	Publish(ctx context.Context, docID string, data []byte) error

	// Subscribe returns a channel of bytes published to a document by other instances,
	// plus a cancel func to unsubscribe.
	Subscribe(ctx context.Context, docID string) (<-chan []byte, func(), error)
}
