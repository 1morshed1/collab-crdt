# 00 — Overview & learning roadmap

Welcome. This document is the map for the whole project. Read it first, then follow
the numbered docs in order. Each one is written to be read _alongside_ the code it
describes.

## What we are building, in one paragraph

A collaborative text editor. Several people open the same document and type into it at
the same time. Everyone sees everyone else's edits within milliseconds. Crucially,
there is no moment where the system "locks" the document or asks a central authority
"what is the correct order of these edits?" Each participant edits their own local copy
freely and instantly; the copies are kept in sync by a data structure whose _math_
guarantees they all end up identical. That data structure is a **CRDT**.

## Why this problem is hard (and worth learning)

Imagine two people. The document says `cat`. At the exact same instant:

- Alice puts her cursor before `c` and types `s` → she sees `scat`.
- Bob puts his cursor after `t` and types `s` → he sees `cats`.

Neither edit "wins" — both should survive. The final document should be `scats` on
both screens. Easy enough here. But now imagine thousands of these happening per second,
arriving out of order, some duplicated, some delayed by a flaky network, while a third
person deletes the `a`. The final document must be **identical on every screen**, and
it must get there **without a central server dictating order**.

The naive approach — "represent the document as a string and send `insert at index 3`"
— breaks immediately, because index 3 means different things on different screens once
concurrent edits shift the text around. This is the core insight that motivates
everything else.

> **The central idea:** stop identifying positions by _integer index_. Give every single
> character a _globally unique, permanent identity_ that never changes even as the text
> around it shifts. Then "insert after character X" is unambiguous forever.

## The two families of solutions (and why we pick CRDT)

| | Operational Transformation (OT) | CRDT |
|---|---|---|
| Used by | Google Docs | Figma, Notion-ish, Yjs, Automerge |
| How | A central server _transforms_ each op against concurrent ops to fix up indices | Ops carry unique IDs and are designed to commute — apply in any order, same result |
| Needs a central authority? | Yes — the server is the source of truth for ordering | No — any replica can apply ops locally and converge |
| Hard part | Getting the transformation functions provably correct (notoriously buggy) | Designing the ID scheme and integration rule; managing "tombstones" |

We build a CRDT. Specifically the **RGA (Replicated Growable Array)** algorithm, which
is one of the most well-understood _sequence_ CRDTs. Doc `01` explains it from scratch.

## The properties we are chasing (the vocabulary)

A CRDT guarantees **Strong Eventual Consistency (SEC)**: any two replicas that have
received the same set of operations are in the same state, regardless of order or
duplication. That decomposes into three requirements on how operations combine:

- **Commutative** — `a then b` == `b then a`. Order of arrival doesn't matter.
- **Associative** — grouping doesn't matter.
- **Idempotent** — applying the same op twice == applying it once. Duplicates are safe.

Keep these three words in mind; every design choice in the CRDT exists to preserve them.

## Architecture at a glance

```
   Browser tab A                         Browser tab B
   ┌───────────────┐                     ┌───────────────┐
   │  editor UI    │                     │  editor UI    │
   │  local RGA    │                     │  local RGA    │   each client owns a full
   │  replica      │                     │  replica      │   CRDT replica; edits apply
   └──────┬────────┘                     └──────┬────────┘   locally *first*
          │  WebSocket (ops in/out)             │
          └───────────────┬─────────────────────┘
                          ▼
              ┌────────────────────────┐
              │   Go collab server     │  one "room" per document
              │  ┌──────────────────┐  │
              │  │ server-side RGA   │  │  authoritative replica; also converges
              │  │ replica per room  │  │
              │  └──────────────────┘  │
              │   broadcast to peers   │
              └───────┬────────┬───────┘
                      │        │
             ┌────────▼──┐  ┌──▼─────────┐
             │ PostgreSQL│  │   Redis    │
             │ op log +  │  │ presence + │
             │ snapshots │  │ pub/sub    │
             └───────────┘  └────────────┘
```

## The build order (and which doc covers each)

| Doc | Phase | What you'll learn |
|-----|-------|-------------------|
| `01-crdt-rga.md` | The CRDT core | Unique IDs, Lamport clocks, tombstones, the RGA integration rule, why it converges |
| `02-operations-and-cursors.md` | Ops & cursors | Serialization, and why cursors must be IDs not offsets |
| `03-websocket-server.md` | Collaboration server | Rooms, the hub pattern, the WS message protocol, concurrency |
| `04-persistence.md` | Postgres | The op log, snapshots/compaction, fast document loads, version history |
| `05-presence-and-redis.md` | Presence | Ephemeral state, heartbeats, and scaling across pods with pub/sub |
| `06-frontend.md` | Client | Wiring a textarea to a CRDT, rendering remote cursors, reconnection |
| `07-running-and-ops.md` | Operations | Docker Compose, config, running & debugging locally |
| `08-testing-and-load.md` | Testing | The randomized convergence test, latency, and scale limits |
| `decisions.md` | (living) | A running log of every non-obvious decision and its rationale |

## How to read this repo if you're learning

1. Read this doc (done!).
2. Read `docs/01-crdt-rga.md`, then read `internal/crdt/*.go` with the tests open.
   The tests are examples — they show the algorithm doing what the prose describes.
3. Run `go test ./internal/crdt -v` and watch convergence pass.
4. Continue down the table. Each layer builds on the one above it.

Everything is committed in small, themed commits so the git history itself is a
narrative you can `git log` through.
