# collab-crdt — a real-time collaborative document editor built on CRDTs

[![CI](https://github.com/1morshed1/collab-crdt/actions/workflows/ci.yml/badge.svg)](https://github.com/1morshed1/collab-crdt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Multiple people edit the same document at the same time. Every keystroke shows up
instantly for everyone. There is **no central lock** and **no operational-transform
server deciding the "true" order of edits** — instead, edits are structured so that
they can be applied _in any order_ on every machine and still converge to exactly the
same document. That is the whole point of a **CRDT (Conflict-free Replicated Data Type)**.

The `docs/` folder walks through _why_ every piece is shaped the way it is. To
understand CRDTs and the systems around them, read the docs in order — they are
numbered.

---

## What's inside

| Layer | Tech | Folder |
|-------|------|--------|
| CRDT core (the RGA algorithm) | Go, zero deps | `internal/crdt` |
| Collaboration server (rooms, broadcast) | Go + WebSocket | `internal/server` |
| Persistence (op log + snapshots) | PostgreSQL | `internal/store`, `migrations` |
| Presence & multi-pod fan-out | Redis pub/sub | `internal/presence` |
| Client (editor UI) | Vanilla JS, zero build | `web` |
| Entry point | wires it all together | `cmd/server` |
| Learning material | **read this** | `docs` |

## The 30-second mental model

```
 You type "H"  ──▶  your browser makes an Insert operation with a globally unique ID
                    and applies it locally *immediately* (no waiting for a server)
                         │
                         ▼  WebSocket
                 ┌──────────────────┐
                 │  Go server (room) │  applies the same op to its own replica,
                 │                   │  writes it to Postgres, and re-broadcasts
                 └──────────────────┘
                         │  WebSocket
                         ▼
 Everyone else receives the op and applies it. Because of how the IDs are
 designed, everyone ends up with the identical document — no matter what order
 the ops arrived in.
```

## Quick start

Absolute fastest (needs only Go — in-memory, single instance):

```bash
make run-mem          # or: go run ./cmd/server
# open http://localhost:8080/?doc=notes in two browser tabs
```

Full stack (Postgres persistence + Redis fan-out) in Docker:

```bash
make up               # builds the app, starts db + redis, serves :8080
```

Or run the infra in Docker and the app on your host for fast iteration:

```bash
make infra-up && make run
```

Type in one tab, watch it appear in the other. Open a third. Disconnect your
network for a second and keep typing — when you reconnect, everything converges.
Run **two server pods** sharing Redis with `make up-multi` (`:8080` and `:8081`)
to watch collaboration work across separate processes.

Requires Go 1.25+ (dependencies are vendored, so the Docker image builds offline).
Run `make help` for every command.

## The learning docs (read in order)

| # | Doc | What you'll learn |
|---|-----|-------------------|
| 00 | [overview](docs/00-overview.md) | The problem, CRDT vs OT, vocabulary, roadmap |
| 01 | [crdt-rga](docs/01-crdt-rga.md) | RGA from first principles: ids, Lamport clocks, tombstones, convergence |
| 02 | [operations-and-cursors](docs/02-operations-and-cursors.md) | Op serialization; why cursors are ids, not offsets |
| 03 | [websocket-server](docs/03-websocket-server.md) | Rooms, the hub, broadcast, concurrency |
| 04 | [persistence](docs/04-persistence.md) | Op log, snapshots, fast reload, version history |
| 05 | [presence-and-redis](docs/05-presence-and-redis.md) | Ephemeral presence; scaling across pods |
| 06 | [frontend](docs/06-frontend.md) | textarea→CRDT diffing, caret preservation, remote cursors |
| 07 | [running-and-ops](docs/07-running-and-ops.md) | Config, Docker, operating it |
| 08 | [testing-and-load](docs/08-testing-and-load.md) | Convergence testing + real performance numbers |
| — | [decisions](docs/decisions.md) | Running log of every non-obvious decision (D1–D8) |

## Running the tests

```bash
make test                 # unit tests, no infra needed (always green)
make test-crdt            # the CRDT verbosely, incl. randomized convergence
make loadtest             # scale/latency harness with printed numbers

make infra-up             # start Postgres + Redis, then:
make test-integration     # everything incl. DB + Redis integration tests, with -race
```

The randomized convergence test is the single most important test in the repo: it
applies thousands of operations in random orders across simulated replicas and
asserts they all reach byte-identical state. If the CRDT has a bug, this catches it.

## Project status

All phases from the original spec are implemented and verified end-to-end: the RGA CRDT,
the WebSocket collaboration server, PostgreSQL persistence with snapshots, Redis
cross-pod fan-out, and the browser editor with live cursors and presence.
Deliberately-scoped extensions (cross-pod presence roster, tombstone garbage
collection, multi-line remote selections, a version-history slider UI) are
documented as such in the docs and decision log rather than left as silent gaps.

## License

[MIT](LICENSE)
