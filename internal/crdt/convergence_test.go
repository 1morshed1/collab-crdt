package crdt

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// This file contains the single most important test in the repository. Everything
// else in the project is plumbing; if the CRDT does not converge, none of it
// matters. So we hammer it: many replicas, thousands of concurrent operations,
// delivered to each other in randomized orders, and we assert that every replica
// ends up byte-identical.
//
// If you ever change the integration rule in document.go, run this test. A subtle
// convergence bug that never shows up in a hand-written unit test will almost always
// be caught here, because random ordering explores the pathological interleavings a
// human wouldn't think to write down.

// TestConvergenceRandomized simulates N replicas. Each replica performs a batch of
// random local edits (inserts and deletes at random positions). Every operation
// produced anywhere is then delivered to every replica in a per-replica random
// order. At the end, all replicas must agree.
func TestConvergenceRandomized(t *testing.T) {
	const (
		replicas   = 5
		opsPerRepl = 400
	)
	rng := rand.New(rand.NewSource(1)) // fixed seed => reproducible failures

	docs := make([]*Document, replicas)
	for i := range docs {
		docs[i] = New(fmt.Sprintf("R%d", i))
	}

	// Phase 1: each replica edits its OWN copy locally, collecting the ops it emits.
	// These edits are "concurrent" in the CRDT sense — no replica has seen any
	// other's ops yet.
	var allOps []Operation
	for i, d := range docs {
		ops := randomEdits(d, opsPerRepl, rng)
		allOps = append(allOps, ops...)
		_ = i
	}

	// Phase 2: deliver every op to every replica, each in its own shuffled order,
	// skipping the replica that already applied it locally when it was generated.
	for i, d := range docs {
		order := rng.Perm(len(allOps))
		for _, k := range order {
			op := allOps[k]
			// Ops this replica authored are already applied; Apply is idempotent so
			// re-applying is harmless, but skipping keeps the test honest about
			// "remote delivery".
			if op.Type == OpInsert && op.ID.Site == d.siteID {
				continue
			}
			d.Apply(op)
		}
		_ = i
	}

	// Phase 3: assert global convergence.
	want := docs[0].Text()
	for i, d := range docs {
		if got := d.Text(); got != want {
			t.Fatalf("replica %d diverged:\n R0 = %q\n R%d = %q", i, want, i, got)
		}
	}
	t.Logf("converged: %d replicas, %d total ops, final length %d",
		replicas, len(allOps), len([]rune(want)))
}

// TestConvergenceManyOrders takes one fixed set of ops and delivers it to a fresh
// replica in many different random orders, asserting the result is always the same.
// This isolates order-independence specifically (Phase 2 above also mixes in the
// authoring replica's own state).
func TestConvergenceManyOrders(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// Build a reference document and capture its full op stream.
	ref := New("ref")
	ops := randomEdits(ref, 300, rng)
	want := ref.Text()

	for trial := 0; trial < 25; trial++ {
		d := New("replay")
		order := rng.Perm(len(ops))
		for _, k := range order {
			d.Apply(ops[k])
		}
		if got := d.Text(); got != want {
			t.Fatalf("trial %d diverged from reference:\n want %q\n got  %q", trial, want, got)
		}
	}
}

// TestConvergenceWithDuplicates delivers each op one to three times in random order
// to confirm idempotency holds under the same conditions as ordering.
func TestConvergenceWithDuplicates(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	ref := New("ref")
	ops := randomEdits(ref, 200, rng)
	want := ref.Text()

	var stream []Operation
	for _, op := range ops {
		reps := 1 + rng.Intn(3)
		for r := 0; r < reps; r++ {
			stream = append(stream, op)
		}
	}
	rng.Shuffle(len(stream), func(i, j int) { stream[i], stream[j] = stream[j], stream[i] })

	d := New("dup")
	for _, op := range stream {
		d.Apply(op)
	}
	if got := d.Text(); got != want {
		t.Fatalf("duplicates changed result:\n want %q\n got  %q", want, got)
	}
}

// randomEdits performs n random local edits on d and returns the ops produced.
// It mixes inserts (weighted heavily) with deletes so tombstones are exercised too.
func randomEdits(d *Document, n int, rng *rand.Rand) []Operation {
	alphabet := []rune("abcdefghijklmnopqrstuvwxyz ")
	var ops []Operation
	for i := 0; i < n; i++ {
		length := d.Len()
		// 20% deletes once there's something to delete.
		if length > 0 && rng.Float64() < 0.2 {
			idx := rng.Intn(length)
			target, ok := d.TargetAtOffset(idx)
			if ok {
				ops = append(ops, d.LocalDelete(target))
			}
			continue
		}
		offset := 0
		if length > 0 {
			offset = rng.Intn(length + 1)
		}
		after := d.OffsetToAfterID(offset)
		ch := string(alphabet[rng.Intn(len(alphabet))])
		ops = append(ops, d.LocalInsert(after, ch))
	}
	return ops
}

// TestSiblingOrderIsTotal is a focused check that the sibling ordering is a strict
// total order (no two distinct concurrent inserts ever tie), which is what the
// convergence proof relies on. We generate many same-parent inserts across sites and
// confirm the resulting order is deterministic regardless of apply order.
func TestSiblingOrderIsTotal(t *testing.T) {
	// Create a shared parent character first.
	base := New("base")
	parentOp := base.LocalInsert(Root, "P")
	parent := parentOp.ID

	// Many sites each insert one char immediately after the parent, all "concurrent".
	var ops []Operation
	for s := 0; s < 30; s++ {
		site := fmt.Sprintf("s%02d", s)
		d := New(site)
		d.Apply(parentOp)
		ops = append(ops, d.LocalInsert(parent, string(rune('a'+s%26))))
	}

	// Apply to two replicas in different orders; the visible order of the siblings
	// must match.
	build := func(seed int64) string {
		d := New("x")
		d.Apply(parentOp)
		order := rand.New(rand.NewSource(seed)).Perm(len(ops))
		for _, k := range order {
			d.Apply(ops[k])
		}
		return d.Text()
	}
	first := build(1)
	for seed := int64(2); seed <= 10; seed++ {
		if got := build(seed); got != first {
			t.Fatalf("sibling order not deterministic across apply orders:\n %q\n %q", first, got)
		}
	}

	// Sanity: the siblings should be sorted by id descending after the parent 'P'.
	// (We don't assert the exact string, just that it's stable, which we did above.)
	if len([]rune(first)) != len(ops)+1 {
		t.Fatalf("expected %d chars, got %d", len(ops)+1, len([]rune(first)))
	}
	_ = sort.Ints
}
