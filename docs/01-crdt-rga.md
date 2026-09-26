# 01 — The CRDT: RGA from first principles

> Code for this doc: `internal/crdt/charid.go`, `operation.go`, `document.go`,
> `snapshot.go`. Tests: `document_test.go`, `convergence_test.go`.

This is the intellectual core of the project. Take your time. By the end you should
be able to explain, to someone else, why concurrent edits converge without a central
coordinator.

## 1. The problem with integer positions

Represent a document as a string and describe an edit as "insert `x` at index 3."
Now two people edit `cat` concurrently:

```
Alice: insert 's' at index 0   ->  she means "before the c"
Bob:   insert 's' at index 3   ->  he means "after the t"
```

Alice's op reaches Bob. He applies "insert 's' at index 0" → `scat`. Good. Bob's op
reaches Alice. She applies "insert 's' at index 3". But on Alice's screen the string
is already `scat`, so index 3 is between `a` and `t`: she gets `scast`. **They
diverged.** The indices meant different things on different machines because earlier
edits shifted them.

You can try to patch this by "transforming" incoming ops against the ops you've
already applied (add 1 to Bob's index because Alice inserted before it). That is
exactly **Operational Transformation**, and getting the transform functions correct
for every pair of op types, in every order, is famously error-prone. We take the
other road.

## 2. The fix: give every character a permanent identity

Instead of "index 3," every character gets a **CharID** the moment it is born, and
that id never changes. An edit is then phrased against ids:

- Insert: "put character `s` **after the character whose id is X**."
- Delete: "remove **the character whose id is Y**."

"After character X" means the same thing on every machine forever, no matter how much
text is inserted or deleted around it. That is the property integer offsets lacked.

See `charid.go`. A `CharID` is:

```go
type CharID struct {
    Lamport uint64  // a logical clock value
    Site    string  // which replica created this character
}
```

### Why a Lamport clock and not a wall clock?

Wall clocks on different machines disagree (by seconds, sometimes minutes). We cannot
use them to order events. A **Lamport clock** is a simple counter with two rules that
together give a consistent notion of "happened-before":

1. **On every local event (an insert), increment your counter** and stamp the event
   with the new value.
2. **When you receive an event, set your counter to `max(yours, theirs)`** before you
   next increment.

Rule 2 ensures that if event A caused event B (B was created after seeing A), then
`A.Lamport < B.Lamport`. It does *not* order truly concurrent events — and that's
fine, because for those we fall back to the `Site` string as a tiebreaker. Together
`(Lamport, Site)` is a **total order**: any two distinct ids compare unambiguously.
See `CharID.Less` and `Document.observe`.

## 3. Uniqueness — why ids never collide

- A single replica increments its Lamport before issuing each id, so it never reuses a
  value → no collisions within a site.
- Two different replicas are distinguished by `Site` even if they pick the same
  Lamport value at the same instant.

So `(Lamport, Site)` is globally unique with **zero coordination**. Nobody has to ask
a server "is this id taken?"

## 4. RGA: how "insert after X" resolves concurrency

RGA stands for **Replicated Growable Array**. The rule is small:

> Each character remembers the id of the character it was inserted **after** (its
> "parent," `Root` for the start of the document). When several characters share the
> same parent (they were inserted at the same spot concurrently), they are ordered
> among themselves by their CharID, **highest id first**.

We model this literally as a **tree** (see decision D3): parent → children, children
kept sorted by id descending. The visible document is a **pre-order depth-first walk**
of that tree, skipping deleted characters. See `document.go`: `applyInsert`,
`insertChildSorted`, `walk`, `Text`.

### Worked example

Type `HI`: `H` is inserted after `Root` (id `1@A`), `I` after `H` (id `2@A`).

```
Root
 └─ H (1@A)
     └─ I (2@A)
```

DFS pre-order → `H`, then `I` → **"HI"**.

Now put the cursor between `H` and `I` and type `X` (id `3@A`). `X`'s parent is `H`,
so `H` now has two children, `I (2@A)` and `X (3@A)`. Sorted **descending**: `X`
first.

```
Root
 └─ H (1@A)
     ├─ X (3@A)
     └─ I (2@A)
```

DFS → `H`, `X`, `I` → **"HXI"**. `X` landed exactly where the cursor was.

### The concurrent case (the whole point)

Two replicas, both insert after `Root` at logical time 1:

- A inserts `H` → id `1@A`
- B inserts `X` → id `1@B`

Both are children of `Root`. Sorted descending, `1@B > 1@A` (same Lamport, `"B" >
"A"`), so `X` comes first: **"XH"** — and this is computed identically on *every*
replica, because the ordering is a pure function of the ids, which everyone agrees on.
No messages, no locks, no server arbitration.

> **Heads-up on `plan.md`:** its prose says this example yields "HX", but its own
> pseudocode (and the standard RGA rule) yields "XH" (higher id first). We follow the
> code/standard and document it in decision D2 and test it in
> `TestConcurrentInsertSameSpot`. The direction is a convention; consistency is what
> matters.

## 5. Deletes and tombstones

When you delete a character, you **do not remove it** from the tree. You set a
`deleted` flag — a **tombstone**. Why keep it? Because another replica may have a
concurrent op that says "insert after that character." If we had physically removed
it, that op would have no anchor to attach to, and it would either be lost or applied
in the wrong place. The tombstone stays as a stable anchor; we just skip it when
rendering text. See `applyDelete` and `Text`.

Tombstones accumulate over time. The cost is bounded by *total historical edits*, not
current document length. `docs/04` covers **compaction/snapshots**, which is how we
eventually reclaim that space once every replica has seen the deletes.

## 6. Order-independence and duplicates (the guarantees, in code)

A CRDT must give **Strong Eventual Consistency**: same ops applied ⇒ same state,
regardless of order or duplication. Two mechanisms in `document.go` deliver this:

- **Idempotency.** `applyInsert` returns early if the id already exists; `applyDelete`
  just sets a flag (setting it twice is the same as once). So re-delivered messages —
  which real networks produce — are harmless. Tested in `TestIdempotentApply` and
  `TestConvergenceWithDuplicates`.

- **Out-of-order readiness.** An insert whose parent hasn't arrived yet, or a delete
  whose target hasn't arrived yet, is **parked** in `pendingChildren` /
  `pendingDeletes` and replayed the instant the referenced character shows up. This
  means we do not depend on the network to deliver ops in causal order — we tolerate
  *any* order. Tested in `TestOutOfOrderDelivery`.

Because the sibling ordering is a **total order** (no ties between distinct ids), the
tree any replica builds is fully determined by the *set* of ops it has received, not
the order it received them in. That is the convergence argument in one sentence.

## 7. How we prove it actually works

Hand-written unit tests only cover interleavings a human thought of. The real
guarantee comes from `convergence_test.go`:

- `TestConvergenceRandomized` — 5 replicas, 400 local edits each (2000 ops total),
  every op delivered to every replica in a **different random order**; asserts all 5
  end byte-identical.
- `TestConvergenceManyOrders` — one fixed op set, replayed in 25 different random
  orders, must always produce the same text.
- `TestConvergenceWithDuplicates` — every op delivered 1–3 times, shuffled.

Run them:

```bash
go test ./internal/crdt -run Convergence -v
go test -race ./internal/crdt          # also confirms the mutex usage is sound
```

If you modify the integration rule, these tests are your safety net. A convergence
bug that no unit test catches will almost always surface here.

## 8. Complexity and honest tradeoffs

This implementation optimizes for **clarity**, not raw speed:

- `Text()`, `Len()`, and the cursor conversions each walk the whole tree: **O(n)** in
  total historical characters (including tombstones). Fine for a learning editor and
  documents up to tens of thousands of characters.
- Insert is **O(children)** for the sorted insert into a parent's child slice, plus a
  map lookup.

Production CRDT libraries (Yjs, Automerge, `diamond-types`) use cleverer structures:
run-length "blocks" of consecutive characters, balanced trees / skip lists for
O(log n) offset lookup, and compact binary encodings. The *algorithm* is the same
idea; the *data structures* are tuned. `docs/08` measures where our simple version
starts to hurt.

## Recap

- Positions are **stable ids**, never integer offsets.
- Ids are `(Lamport, Site)`: unique and totally ordered with **no coordination**.
- Insert = "attach as a child of the anchor"; concurrent inserts at one spot are
  ordered by id. Delete = tombstone, never unlink.
- Idempotency + out-of-order parking ⇒ order- and duplicate-independent ⇒ convergence.
- We prove convergence with randomized multi-replica tests.

Next: `docs/02-operations-and-cursors.md` — how ops are serialized, and why cursors
must also be ids.
