// Package store is the PostgreSQL persistence layer. It implements the server.Store
// interface: an append-only operation log plus periodic snapshots, which together let a
// document survive server restarts and load quickly. See docs/04.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/1morshed1/collab-crdt/internal/crdt"
)

// Store wraps a pgx connection pool and provides document persistence.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres using a standard connection string
// (e.g. "postgres://user:pass@host:5432/dbname"), verifies connectivity, and applies
// any pending migrations. The returned Store is ready to use.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// EnsureDocument inserts a documents row if it does not already exist. The ON CONFLICT
// clause makes it a safe no-op for an existing document — several rooms/pods may call
// this concurrently for the same id.
func (s *Store) EnsureDocument(ctx context.Context, docID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO documents(id) VALUES($1) ON CONFLICT (id) DO NOTHING`, docID)
	return err
}

// AppendOp records one operation and returns the DB seq assigned to it. The seq is the
// monotonic boundary marker used everywhere else in persistence (see the Store
// interface docs). op_type is stored as a separate column purely for easy querying /
// debugging; the authoritative data is the JSON blob.
func (s *Store) AppendOp(ctx context.Context, docID string, op crdt.Operation) (int64, error) {
	data, err := json.Marshal(op)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO operations_log(document_id, op_type, op_data)
		 VALUES($1, $2, $3) RETURNING seq`,
		docID, string(op.Type), data,
	).Scan(&seq)
	return seq, err
}

// OpsSince returns every operation with seq greater than afterSeq, ascending, plus the
// greatest seq observed. If there are no newer ops, latestSeq == afterSeq.
func (s *Store) OpsSince(ctx context.Context, docID string, afterSeq int64) ([]crdt.Operation, int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, op_data FROM operations_log
		 WHERE document_id=$1 AND seq > $2 ORDER BY seq ASC`,
		docID, afterSeq)
	if err != nil {
		return nil, afterSeq, err
	}
	defer rows.Close()

	latest := afterSeq
	var ops []crdt.Operation
	for rows.Next() {
		var seq int64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, latest, err
		}
		var op crdt.Operation
		if err := json.Unmarshal(data, &op); err != nil {
			return nil, latest, err
		}
		ops = append(ops, op)
		if seq > latest {
			latest = seq
		}
	}
	return ops, latest, rows.Err()
}

// LoadSnapshot returns the newest snapshot for a document (its serialized state and the
// seq it covers), or ok=false if none exists yet.
func (s *Store) LoadSnapshot(ctx context.Context, docID string) ([]byte, int64, bool, error) {
	var data []byte
	var upToSeq int64
	err := s.pool.QueryRow(ctx,
		`SELECT crdt_state, up_to_seq FROM document_snapshots
		 WHERE document_id=$1 ORDER BY up_to_seq DESC LIMIT 1`,
		docID,
	).Scan(&data, &upToSeq)
	if err != nil {
		// pgx returns pgx.ErrNoRows when there is no snapshot; treat that as "none".
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	return data, upToSeq, true, nil
}

// SaveSnapshot stores a new snapshot and prunes older snapshots for the same document
// (we only ever need the newest one to load). We KEEP the operation log intact so
// version history / time-travel remains possible — see docs/04 and docs/05 on the
// separate question of tombstone garbage collection.
func (s *Store) SaveSnapshot(ctx context.Context, docID string, data []byte, upToSeq int64, opCount int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.Exec(ctx,
		`INSERT INTO document_snapshots(document_id, crdt_state, up_to_seq, op_count)
		 VALUES($1, $2, $3, $4)`,
		docID, data, upToSeq, opCount,
	); err != nil {
		return err
	}
	// Keep only the latest snapshot per document.
	if _, err := tx.Exec(ctx,
		`DELETE FROM document_snapshots
		 WHERE document_id=$1 AND up_to_seq < $2`,
		docID, upToSeq,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Ensure *Store satisfies the server.Store interface at compile time. We can't import
// the server package here (that would be a cycle), so this assertion lives in the
// server package's wiring instead; see cmd/server.
