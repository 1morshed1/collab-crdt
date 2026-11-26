package crdt

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// This file is the load / performance harness. Unlike convergence_test.go (which asserts
// CORRECTNESS under random ordering), these tests measure BEHAVIOR AT SCALE and print
// numbers you can read. Run them with:
//
//	go test ./internal/crdt -run TestLoad -v
//
// They double as executable versions of the original spec's "Load Testing" section.

// TestLoadConvergence5x1000 is exactly the spec's headline correctness-at-scale test:
// 5 concurrent clients each insert 1,000 characters at random positions, then every op is
// delivered to every replica in a random order. All 5 must end byte-identical.
func TestLoadConvergence5x1000(t *testing.T) {
	const replicas, per = 5, 1000
	rng := rand.New(rand.NewSource(2024))

	docs := make([]*Document, replicas)
	for i := range docs {
		docs[i] = New(fmt.Sprintf("client-%d", i))
	}

	start := time.Now()
	var allOps []Operation
	for _, d := range docs {
		allOps = append(allOps, randomEdits(d, per, rng)...)
	}
	genElapsed := time.Since(start)

	start = time.Now()
	for _, d := range docs {
		for _, k := range rng.Perm(len(allOps)) {
			op := allOps[k]
			if op.Type == OpInsert && op.ID.Site == d.siteID {
				continue // already applied locally
			}
			d.Apply(op)
		}
	}
	applyElapsed := time.Since(start)

	want := docs[0].Text()
	for i, d := range docs {
		if d.Text() != want {
			t.Fatalf("replica %d diverged", i)
		}
	}

	t.Logf("5x1000 convergence: %d ops generated in %v, cross-applied in %v, final len=%d",
		len(allOps), genElapsed.Round(time.Millisecond), applyElapsed.Round(time.Millisecond),
		len([]rune(want)))
}

// TestLoadDocumentScaling measures how the cost of a single operation grows with document
// size — the "Document size" bullet in the original spec. It builds documents of increasing size and
// times a batch of inserts at the end, reporting per-op cost. This makes the O(n) nature of
// our clarity-first implementation visible (see docs/01 §8 and docs/08).
func TestLoadDocumentScaling(t *testing.T) {
	sizes := []int{1000, 10000, 100000}
	for _, n := range sizes {
		d := New("A")
		after := Root
		// Build a document of n characters.
		buildStart := time.Now()
		for i := 0; i < n; i++ {
			op := d.LocalInsert(after, "x")
			after = op.ID
		}
		buildElapsed := time.Since(buildStart)

		// Time 1,000 appends at the end of the now-large document.
		const sample = 1000
		opStart := time.Now()
		for i := 0; i < sample; i++ {
			op := d.LocalInsert(after, "y")
			after = op.ID
		}
		perOp := time.Since(opStart) / sample

		// Time a full text render (an O(n) tree walk).
		renderStart := time.Now()
		_ = d.Text()
		renderElapsed := time.Since(renderStart)

		t.Logf("size=%-7d build=%-8v  per-insert=%-10v  full-render=%v",
			n, buildElapsed.Round(time.Millisecond), perOp.Round(time.Microsecond),
			renderElapsed.Round(time.Microsecond))
	}
}

// TestLoadTombstoneCost shows the cost of tombstones: a document that has had many
// characters inserted and deleted still carries all of them, so operations that walk the
// tree scale with TOTAL edits, not visible length. This is the honest tradeoff from
// docs/01 §5 and the motivation for compaction (docs/04) and tombstone GC (docs/05).
func TestLoadTombstoneCost(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	d := New("A")
	after := Root
	// Insert 20k characters.
	for i := 0; i < 20000; i++ {
		op := d.LocalInsert(after, "x")
		after = op.ID
	}
	// Delete 90% of them at random.
	deleted := 0
	for i := 0; i < 20000; i++ {
		if rng.Float64() < 0.9 {
			if id, ok := d.TargetAtOffset(rng.Intn(maxInt(1, d.Len()))); ok {
				d.LocalDelete(id)
				deleted++
			}
		}
	}
	visible := d.Len()
	renderStart := time.Now()
	_ = d.Text()
	t.Logf("tombstones: visible=%d, deleted=%d, nodes-in-tree=%d, render=%v (render walks tombstones too)",
		visible, deleted, len(d.nodes)-1, time.Since(renderStart).Round(time.Microsecond))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
