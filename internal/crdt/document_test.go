package crdt

import (
	"testing"
)

// typeString is a test helper: it types s into doc one character at a time,
// starting after `after`, exactly like a person typing left to right. It returns
// the ops produced (so another replica can replay them) and the id of the last
// character typed (the new cursor anchor).
func typeString(d *Document, after CharID, s string) ([]Operation, CharID) {
	var ops []Operation
	cursor := after
	for _, r := range s {
		op := d.LocalInsert(cursor, string(r))
		ops = append(ops, op)
		cursor = op.ID
	}
	return ops, cursor
}

func TestInsertSequential(t *testing.T) {
	d := New("A")
	typeString(d, Root, "hello")
	if got := d.Text(); got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

func TestInsertInMiddle(t *testing.T) {
	d := New("A")
	_, cursor := typeString(d, Root, "ac")
	// After typing "ac", cursor is after 'c'. To insert 'b' between a and c, we need
	// the anchor "after a". Offset 1 sits between a(0) and c(1).
	afterA := d.OffsetToAfterID(1)
	d.LocalInsert(afterA, "b")
	if got := d.Text(); got != "abc" {
		t.Fatalf("want %q, got %q", "abc", got)
	}
	_ = cursor
}

func TestDelete(t *testing.T) {
	d := New("A")
	typeString(d, Root, "hello")
	// Delete the 'e' (visible index 1).
	target, ok := d.TargetAtOffset(1)
	if !ok {
		t.Fatal("expected a target at offset 1")
	}
	d.LocalDelete(target)
	if got := d.Text(); got != "hllo" {
		t.Fatalf("want %q, got %q", "hllo", got)
	}
	if d.Len() != 4 {
		t.Fatalf("want len 4, got %d", d.Len())
	}
}

// TestConcurrentInsertSameSpot is the canonical CRDT example: two replicas insert a
// character at the exact same location concurrently. Both characters must survive,
// in the same order, on both replicas.
func TestConcurrentInsertSameSpot(t *testing.T) {
	a := New("A")
	b := New("B")

	// Both replicas insert after Root at logical time 1.
	opA := a.LocalInsert(Root, "H") // id {1, A}
	opB := b.LocalInsert(Root, "X") // id {1, B}

	// Cross-deliver.
	a.Apply(opB)
	b.Apply(opA)

	// Higher id first: {1,B} > {1,A}, so "X" comes before "H" -> "XH" on both.
	if a.Text() != b.Text() {
		t.Fatalf("replicas diverged: A=%q B=%q", a.Text(), b.Text())
	}
	if a.Text() != "XH" {
		t.Fatalf("want %q, got %q (see decision D2 on sibling ordering)", "XH", a.Text())
	}
}

// TestIdempotentApply asserts that delivering the same op twice is harmless. Real
// networks duplicate messages; the CRDT must not care.
func TestIdempotentApply(t *testing.T) {
	a := New("A")
	op := a.LocalInsert(Root, "z")
	a.Apply(op) // duplicate of a local op
	a.Apply(op) // and again
	if a.Text() != "z" {
		t.Fatalf("duplicate apply changed text: got %q", a.Text())
	}
}

// TestOutOfOrderDelivery delivers a child insert BEFORE its parent, and a delete
// BEFORE its target. The document must still converge once everything arrives.
func TestOutOfOrderDelivery(t *testing.T) {
	src := New("A")
	ops, _ := typeString(src, Root, "abc") // ops for a, b, c (each parented on prev)
	delOp := src.LocalDelete(ops[1].ID)    // delete 'b'

	dst := New("B")
	// Deliver in a deliberately broken order: delete first, then c, then b, then a.
	dst.Apply(delOp)  // target 'b' unknown -> parked
	dst.Apply(ops[2]) // 'c', parent 'b' unknown -> parked
	dst.Apply(ops[1]) // 'b', parent 'a' unknown -> parked (also honors parked delete)
	dst.Apply(ops[0]) // 'a', parent Root -> unblocks b, then c

	if dst.Text() != src.Text() {
		t.Fatalf("diverged: src=%q dst=%q", src.Text(), dst.Text())
	}
	if dst.Text() != "ac" {
		t.Fatalf("want %q, got %q", "ac", dst.Text())
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	d := New("A")
	ops, _ := typeString(d, Root, "hello world")
	d.LocalDelete(ops[4].ID) // delete the 'o' in hello -> "hell world"

	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	loaded, err := LoadSnapshot(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Text() != d.Text() {
		t.Fatalf("round trip changed text: %q -> %q", d.Text(), loaded.Text())
	}

	// The restored clock must let us keep generating ids without collision.
	op := loaded.LocalInsert(Root, "!")
	if op.ID.Lamport <= ops[len(ops)-1].ID.Lamport {
		t.Fatalf("clock not restored: new id %v not ahead of %v", op.ID, ops[len(ops)-1].ID)
	}
}

func TestCursorAnchorSurvivesConcurrentEdits(t *testing.T) {
	d := New("A")
	typeString(d, Root, "hello")
	// Cursor sits after "he" (offset 2).
	anchor := d.OffsetToAfterID(2)
	if off := d.AfterIDToOffset(anchor); off != 2 {
		t.Fatalf("want offset 2, got %d", off)
	}
	// Someone inserts "XYZ" at the very start. A raw integer offset of 2 would now
	// point into the wrong place; the anchor must still resolve to just after "he".
	after := Root
	for _, r := range "XYZ" {
		op := d.LocalInsert(after, string(r))
		after = op.ID
	}
	if got := d.Text(); got != "XYZhello" {
		t.Fatalf("want %q, got %q", "XYZhello", got)
	}
	if off := d.AfterIDToOffset(anchor); off != 5 { // "XYZhe" -> 5
		t.Fatalf("anchor drifted: want offset 5, got %d", off)
	}
}
