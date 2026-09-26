package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/1morshed1/collab-crdt/internal/crdt"
)

// These are integration tests: they need a real Postgres. Set TEST_DATABASE_URL to run
// them, e.g.
//
//	TEST_DATABASE_URL=postgres://collab:collab@localhost:5433/collab go test ./internal/store
//
// Without it, they skip — so `go test ./...` stays green on a machine with no database.

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run store integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// uniqueDoc returns a fresh document id per test run so tests don't collide when run
// repeatedly against the same database.
func uniqueDoc(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestOpLogRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc := uniqueDoc("oplog")

	if err := s.EnsureDocument(ctx, doc); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Append a few ops and confirm seq is monotonically increasing.
	var lastSeq int64
	ids := []crdt.CharID{}
	for i, ch := range []string{"h", "i", "!"} {
		after := crdt.Root
		if i > 0 {
			after = ids[i-1]
		}
		id := crdt.CharID{Lamport: uint64(i + 1), Site: "A"}
		ids = append(ids, id)
		seq, err := s.AppendOp(ctx, doc, crdt.NewInsert(id, after, ch))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if seq <= lastSeq {
			t.Fatalf("seq not increasing: %d after %d", seq, lastSeq)
		}
		lastSeq = seq
	}

	// Read them all back and rebuild a document; it must say "hi!".
	ops, latest, err := s.OpsSince(ctx, doc, 0)
	if err != nil {
		t.Fatalf("opssince: %v", err)
	}
	if latest != lastSeq {
		t.Fatalf("latest seq %d != %d", latest, lastSeq)
	}
	d := crdt.New("replay")
	for _, op := range ops {
		d.Apply(op)
	}
	if d.Text() != "hi!" {
		t.Fatalf("rebuilt text = %q, want %q", d.Text(), "hi!")
	}

	// OpsSince with a boundary should return only newer ops.
	newer, _, err := s.OpsSince(ctx, doc, lastSeq-1)
	if err != nil {
		t.Fatalf("opssince boundary: %v", err)
	}
	if len(newer) != 1 {
		t.Fatalf("want 1 op after boundary, got %d", len(newer))
	}
}

func TestSnapshotLoadAndPrune(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc := uniqueDoc("snap")
	if err := s.EnsureDocument(ctx, doc); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// No snapshot yet.
	if _, _, ok, err := s.LoadSnapshot(ctx, doc); err != nil || ok {
		t.Fatalf("expected no snapshot, ok=%v err=%v", ok, err)
	}

	// Build a document, snapshot it.
	d := crdt.New("A")
	after := crdt.Root
	for _, r := range "hello" {
		op := d.LocalInsert(after, string(r))
		after = op.ID
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := s.SaveSnapshot(ctx, doc, data, 5, 5); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// A newer snapshot should replace (prune) the older one.
	for _, r := range " world" {
		op := d.LocalInsert(after, string(r))
		after = op.ID
	}
	data2, _ := d.Snapshot()
	if err := s.SaveSnapshot(ctx, doc, data2, 11, 6); err != nil {
		t.Fatalf("save snapshot 2: %v", err)
	}

	loaded, upTo, ok, err := s.LoadSnapshot(ctx, doc)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if upTo != 11 {
		t.Fatalf("want upToSeq 11, got %d", upTo)
	}
	restored, err := crdt.LoadSnapshot(loaded)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Text() != "hello world" {
		t.Fatalf("restored text = %q", restored.Text())
	}

	// Confirm only the newest snapshot remains.
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM document_snapshots WHERE document_id=$1`, doc,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 snapshot after prune, got %d", count)
	}
}

// TestFullReloadPath simulates a server restart: snapshot at some point, more ops after,
// then reconstruct exactly as Room.loadFromStore does (snapshot + OpsSince).
func TestFullReloadPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc := uniqueDoc("reload")
	if err := s.EnsureDocument(ctx, doc); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Author "abc", log each op, snapshot after "ab".
	d := crdt.New("A")
	after := crdt.Root
	var seqs []int64
	for _, r := range "abc" {
		op := d.LocalInsert(after, string(r))
		after = op.ID
		seq, err := s.AppendOp(ctx, doc, op)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		seqs = append(seqs, seq)
	}
	// Snapshot state as-of the first two ops only (pretend 'c' came after the snapshot).
	partial := crdt.New("A")
	after = crdt.Root
	for _, r := range "ab" {
		op := partial.LocalInsert(after, string(r))
		after = op.ID
	}
	snapData, _ := partial.Snapshot()
	if err := s.SaveSnapshot(ctx, doc, snapData, seqs[1], 2); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// Now reconstruct the way the Room does.
	data, upTo, ok, err := s.LoadSnapshot(ctx, doc)
	if err != nil || !ok {
		t.Fatalf("load snapshot: ok=%v err=%v", ok, err)
	}
	rebuilt, err := crdt.LoadSnapshot(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ops, _, err := s.OpsSince(ctx, doc, upTo)
	if err != nil {
		t.Fatalf("opssince: %v", err)
	}
	for _, op := range ops {
		rebuilt.Apply(op)
	}
	if rebuilt.Text() != "abc" {
		t.Fatalf("reload produced %q, want %q", rebuilt.Text(), "abc")
	}
}
