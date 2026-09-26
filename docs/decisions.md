# Decision log

A running, append-only log of every non-obvious choice made while building this project,
with the reasoning at the time. This is deliberately informal — it's the "why" that
code comments and commit messages don't have room for. Newest entries at the bottom of
each section.

The format for each entry: **the decision**, then **why**, then **what we gave up**.

---

## D1 — Use a CRDT (RGA), not Operational Transformation

**Decision:** Implement the RGA (Replicated Growable Array) sequence CRDT.

**Why:** The project's whole learning goal is convergence without central ordering. OT
requires a server to transform operations and is famously hard to get provably correct.
RGA is well-documented, its convergence argument is understandable, and it lets any
replica apply ops locally and immediately. RGA specifically (vs. other sequence CRDTs
like Logoot/LSEQ/Treedoc) because its "insert after a stable reference ID" model maps
cleanly onto how a text editor thinks and onto stable cursor positions.

**What we gave up:** RGA keeps deleted characters around as "tombstones" forever (until
garbage-collected), so memory grows with total edits, not current length. OT has no
tombstones. We accept this and add snapshot/compaction later.

---

## D2 — Sibling ordering: higher CharID goes first (leftmost)

**Decision:** Among characters inserted after the _same_ reference character, order them
by CharID **descending** (larger ID = earlier in the text).

**Why:** It's the standard RGA convention and it makes concurrent inserts at the same
spot deterministic. The exact direction (ascending vs descending) is arbitrary for
_correctness_ — any total, agreed-upon order converges — but it must be consistent
everywhere. Note: `plan.md`'s prose says "HX" for a concurrent example while its own
pseudocode produces "XH" (higher-ID-first). We follow the **code's** behavior
(higher-ID-first) and document the discrepancy so it doesn't confuse anyone. See
`docs/01-crdt-rga.md`.

**What we gave up:** Nothing meaningful. Just had to pick a lane and be consistent.

---

## D3 — Represent the document as an explicit tree, linearized by DFS

**Decision:** Model the RGA as a tree: each character node hangs off the character it was
inserted after (its "parent"), children are kept sorted by CharID descending, and the
visible text is a pre-order depth-first traversal. This is instead of the fragile
linear-array-with-skip-scan shown in `plan.md`.

**Why:** The tree makes the integration rule trivially correct and easy to reason about:
"insert after X" = "add as a child of X, in sorted order." The array-scan version in the
plan has subtle descendant-skipping logic that is easy to get wrong. Correctness and
teachability beat micro-optimization here, and we back it with randomized convergence
tests.

**What we gave up:** Some performance and memory locality. A production system (Yjs,
Automerge) uses more sophisticated structures (blocks, run-length encoding). We note the
complexity tradeoffs in the CRDT doc but keep the clear version.

---

## D4 — Frontend is zero-build vanilla JS, not React

**Decision:** The client is plain HTML/CSS/JS modules served straight from the Go binary,
no bundler, no npm build step. (`plan.md` suggested React.)

**Why:** This is a learning repo. A build toolchain (Vite/webpack/JSX transpilation) adds
a lot of moving parts that have nothing to do with CRDTs. Vanilla JS keeps the client
transparent — you can read exactly what talks to the WebSocket and how ops are formed,
with no framework indirection. It also means `git clone && run` works with only Go
installed.

**What we gave up:** Ergonomics for a _large_ UI. For this focused editor it's a win;
for a real product you'd want a framework.

---

## D5 — A client loads shared state but keeps its OWN site id (`LoadSnapshotAs`)

**Decision:** When a client joins, the server ships it a snapshot of the document. The
client must rebuild its replica using the **client id the server assigned it** as the
CRDT site — not the site id stored inside the snapshot. `crdt.LoadSnapshotAs(data,
siteID)` exists for exactly this.

**Why (this was a real bug caught by the end-to-end test):** The first version had every
client call `LoadSnapshot`, which adopts the snapshot's stored site id (the server's).
So two tabs both ended up with site `server:<doc>` and the same starting Lamport. The
next character each of them typed got the **identical** CharID `{1, server:<doc>}`. The
idempotency check in `applyInsert` ("already have this id? ignore") then silently
dropped one of the two concurrent edits — the tabs stopped converging. The fix is to
guarantee each replica has a unique site id, which is the whole reason CharIDs carry a
site in the first place. The server-assigned client id is unique, so the client uses it.

**What we gave up:** Nothing. This is just correct id hygiene. The lesson worth keeping:
CRDT idempotency turns an id-uniqueness violation into *silent* data loss rather than a
crash — so uniqueness of the site id is a load-bearing invariant, and it's now tested
by `TestWebSocketConvergenceTwoWay`.

---

## D6 — Drop slow clients rather than block the room

**Decision:** Each client has a bounded outbound buffer (256 messages). If a broadcast
can't enqueue because the buffer is full, we disconnect that client instead of blocking.

**Why:** A room broadcasts every op to every client while holding the room lock. If one
client's socket is slow or wedged, blocking on it would stall broadcasts to *everyone*
and back up the whole room. Availability for the many beats a guaranteed delivery to the
one straggler — and the straggler simply reconnects and reloads a fresh snapshot, so it
loses nothing permanent.

**What we gave up:** A momentarily slow client can be disconnected under heavy load. The
recovery (reconnect + snapshot) makes this safe, and it's the standard hub/broadcast
pattern for exactly this reason.

---

## D7 — Redis Pub/Sub for cross-pod ops/cursors; roster stays per-pod (for now)

**Decision:** Implement the `server.Bus` seam with Redis Pub/Sub to fan out operations
and cursor updates across server instances, with per-instance echo suppression. Leave the
presence *roster* (the "who's online" list) per-pod for now, documenting the Redis-set +
TTL-heartbeat design as the next step.

**Why:** Cross-pod delivery of edits is the substantive distributed-systems requirement
from the plan's scaling section, and it's what actually breaks collaboration when you add
a second pod. Pub/Sub is a perfect fit because the CRDT already tolerates the unordered,
at-most-once delivery Pub/Sub provides — so the transport can stay dumb. The full
cross-pod roster (with crash-safe expiry) is a meaningful additional chunk of state
management; rather than ship a half-correct version that leaks stale users when a pod
dies, we implemented the load-bearing part (editing + live cursors work cluster-wide) and
wrote down precisely how to complete the roster.

**What we gave up:** The online-user count can undercount when editors are split across
pods. Editing correctness and live remote cursors are unaffected. The extension is
additive and documented in docs/05.
