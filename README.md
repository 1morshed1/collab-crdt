# collab-crdt — a real-time collaborative document editor built on CRDTs

Multiple people edit the same document at the same time. Every keystroke shows up
instantly for everyone. There is **no central lock** and **no operational-transform
server deciding the "true" order of edits** — instead, edits are structured so that
they can be applied _in any order_ on every machine and still converge to exactly the
same document. That is the whole point of a **CRDT (Conflict-free Replicated Data Type)**.

This repository is built as a **learning project**. The code is real and it works,
but just as important is the `docs/` folder, which walks through _why_ every piece is
shaped the way it is. If you are here to learn CRDTs and the systems around them, read
the docs in order — they are numbered.

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

```bash
# 1. Start Postgres + Redis
docker compose up -d

# 2. Run the server (applies DB migrations on boot)
make run          # or: go run ./cmd/server

# 3. Open the editor in two browser tabs
open http://localhost:8080
```

Type in one tab, watch it appear in the other. Open a third. Disconnect your
network for a second and keep typing — when you reconnect, everything converges.

See [`docs/00-overview.md`](docs/00-overview.md) for the full learning roadmap.

## Running the tests

```bash
make test                 # everything
go test ./internal/crdt   # just the CRDT, including the randomized convergence tests
```

The randomized convergence test is the single most important test in the repo: it
applies thousands of operations in random orders across simulated replicas and
asserts they all reach byte-identical state. If the CRDT has a bug, this catches it.
