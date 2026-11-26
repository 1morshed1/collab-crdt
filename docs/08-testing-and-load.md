# 08 — Testing correctness and measuring performance

> Code: `internal/crdt/convergence_test.go`, `internal/crdt/load_test.go`,
> `internal/server/server_test.go`, `internal/server/multipod_test.go`,
> `internal/store/store_test.go`, `internal/presence/bus_test.go`.

A CRDT project lives or dies on one question: **do all replicas really converge?** This
doc explains how we test that (correctness), and then how we measure it at scale
(performance). The numbers below are real, from `go test` on the development machine —
run them yourself and you'll get your own.

## The testing pyramid we built

| Layer | File | Needs infra? | What it proves |
|-------|------|--------------|----------------|
| CRDT unit | `crdt/document_test.go` | no | Each rule (insert/delete/cursor/snapshot) in isolation |
| **CRDT convergence** | `crdt/convergence_test.go` | no | Randomized multi-replica convergence — the core guarantee |
| CRDT load | `crdt/load_test.go` | no | Behavior & cost at scale (numbers below) |
| Server e2e | `server/server_test.go` | no | Ops relay + converge over real WebSockets |
| Multi-pod | `server/multipod_test.go` | Redis | Two pods converge via Redis |
| Store | `store/store_test.go` | Postgres | Op log + snapshot + reload round-trips |
| Bus | `presence/bus_test.go` | Redis | Pub/sub delivery, echo suppression, isolation |

Infra-dependent tests **skip** (not fail) when their env var is unset, so
`go test ./...` is always green with nothing installed. Turn them on with
`make test-integration` after `make infra-up`.

## Why randomized convergence testing is the whole game

You cannot prove convergence by hand-picking a few interleavings — the bugs hide in the
orderings you didn't think of. So `convergence_test.go` generates thousands of operations
and delivers them to each simulated replica in a **different random order** (and, in one
variant, with random **duplicates**), then asserts every replica is byte-identical. A
fixed RNG seed makes any failure reproducible.

This approach earned its keep: the site-id collision bug (decision **D5**) — where two
clients generated identical CharIDs and idempotency silently dropped one edit — was caught
by the two-way convergence test, not by any single-scenario unit test. Random,
multi-replica testing turns "silently wrong" into "loudly failing," which is exactly what
you want from a CRDT test suite.

Run it:

```bash
go test ./internal/crdt -run Convergence -v
go test -race ./internal/crdt          # also validates the locking
```

## Load results (from `go test ./internal/crdt -run TestLoad -v`)

### Convergence at scale — the original spec's headline test

`TestLoadConvergence5x1000`: 5 clients, 1,000 random edits each, all cross-applied in
random order.

```
5x1000 convergence: 5000 ops generated in 61ms, cross-applied in 18ms, final len=2874
```

All 5 replicas converged. (Final length is < 5000 because `randomEdits` mixes in deletes.)

### Cost vs document size

`TestLoadDocumentScaling`: build documents of increasing size, then time inserts and a full
render.

```
size=1000     build=0ms     per-insert≈0µs     full-render=140µs
size=10000    build=2ms     per-insert≈0µs     full-render=1.6ms
size=100000   build=43ms    per-insert=1µs     full-render=19ms
```

Reading this honestly (see docs/01 §8): a single **insert** stays cheap (map lookup + a
sorted insert into one parent's child list). The **full render** is O(n) — it walks the
whole tree — so it grows linearly: ~140µs at 1k, ~19ms at 100k. For a live editor that
re-renders on every remote op, 19ms at 100k characters is noticeable but usable; beyond
that you'd want the production optimizations below.

### The price of tombstones

`TestLoadTombstoneCost`: insert 20k characters, delete ~90% of them.

```
tombstones: visible=1998, deleted=18002, nodes-in-tree=20000, render=204µs
             (render walks tombstones too)
```

The tree still holds all 20,000 nodes even though only ~2,000 are visible. This is the
tombstone tradeoff from docs/01 §5 made concrete: memory and walk-cost scale with **total
historical edits**, not current length. Snapshots (docs/04) bound op *replay* on load;
reclaiming the tombstones themselves needs **garbage collection** once all replicas have
seen the deletes (docs/05) — deliberately left as a documented extension.

## Latency and concurrent-editor scale (how to measure)

- **Keystroke latency** (spec's <50ms local / <200ms internet target) is an end-to-end
  property best measured in the browser: timestamp on keydown, timestamp when the character
  appears in the other tab. Locally it's dominated by the WebSocket round-trip, not the
  CRDT (per-op apply is microseconds, as shown above), so on localhost you're comfortably
  under 50ms. The headless smoke tests during development confirmed edits appear "instantly"
  across tabs.
- **Concurrent editors per document** is bounded by broadcast fan-out: each op is sent to
  every other client. At N editors that's O(N) sends per op and O(N²) traffic for N
  simultaneous typers. The slow-client-drop policy (decision **D6**) keeps one straggler
  from stalling the room. Redis fan-out (docs/05) spreads editors across pods to push the
  ceiling higher. Measuring the exact ceiling means spinning up N WebSocket clients against
  a running server — the `server_test.go` harness (`dial`, `typeAtEnd`) is the building
  block for such a script.

## What a production system would add (and where we said so)

- Run-length **block encoding** of consecutive characters and a balanced index for
  **O(log n)** offset lookups instead of O(n) walks (docs/01 §8).
- **Tombstone GC** via version vectors of acknowledged ops (docs/05).
- Cross-pod **presence roster** with TTL heartbeats (docs/05, D7).
- Compact **binary op/snapshot encoding** instead of JSON (docs/04).

We chose clarity and correctness first, measured where that hurts, and wrote down the
path. That's the honest engineering the whole repo aims to teach.

This is the last numbered doc. See `docs/decisions.md` for the running rationale log, and
`docs/00-overview.md` to start over from the top.
