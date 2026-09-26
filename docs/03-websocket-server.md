# 03 — The collaboration server: rooms, hub, and broadcast

> Code: `internal/server/*.go`. Tests: `internal/server/server_test.go`.

The CRDT (docs 01–02) guarantees that if every replica sees the same ops, they all
converge. The server's only job is to make that happen: get each client's op to every
other client, keep an authoritative replica, and (later) persist history. It adds no
"correctness" of its own — even if the server reordered or duplicated messages, the CRDT
would still converge. That's a lovely property: the transport can be dumb.

## The three types and how they relate

```
Server (one process)
 └─ Hub                     one per process; maps docID -> Room
     ├─ Room "notes"        one per open document; owns the server-side CRDT replica
     │   ├─ Client tab-A    one per WebSocket connection
     │   ├─ Client tab-B
     │   └─ Client tab-C
     └─ Room "todo"
         └─ Client tab-D
```

- **Client** (`client.go`) — one WebSocket connection = one browser tab. Its id doubles
  as that tab's CRDT site id.
- **Room** (`room.go`) — one open document. All clients editing it share this. It owns
  the authoritative server-side replica and is the single place where
  "apply → persist → broadcast" happens.
- **Hub** (`hub.go`) — the registry. Guarantees exactly one Room per document, creating
  rooms on demand and retiring them when empty.
- **Server** (`server.go`) — the thin HTTP front door: serves the static UI and upgrades
  `/ws` requests into clients.

## The message protocol

Every frame is a JSON envelope (`protocol.go`):

```json
{ "type": "op", "data": { ...operation... } }
```

| Type | Direction | Payload | Meaning |
|------|-----------|---------|---------|
| `init` | server → client | `InitData` | On connect: your assigned id + a snapshot + the current roster |
| `op` | both ways | `crdt.Operation` | An edit. Client sends its edits; server relays peers' edits. |
| `cursor` | both ways | `CursorData` | Where a caret/selection is (as CharIDs). Ephemeral. |
| `presence` | server → client | `PresenceData` | The full roster; sent whenever someone joins/leaves. |

Two deliberate choices:

1. **The server assigns the client id** (`newClientID` in `server.go`) and sends it in
   `init`. The client must use it as its CRDT site id. If clients picked their own, two
   could collide, and CRDT idempotency would then _silently drop_ one client's edits.
   This actually happened during development — see decision **D5** and doc 01 §3.
2. **Identity comes from the connection, not the payload.** `handleCursor` overwrites
   `cur.ClientID = sender.id`. A client can't claim to be someone else.

## The connection lifecycle (two goroutines per client)

gorilla/websocket requires that only one goroutine writes to a socket and one reads. So
each client runs:

- **`readPump`** — the only reader. Loops `ReadMessage`, decodes the envelope, dispatches
  to `room.handleOp` / `room.handleCursor`. On any read error (tab closed, network gone)
  it calls `room.remove` and returns.
- **`writePump`** — the only writer. Drains the client's buffered `send` channel to the
  socket, and sends a periodic **ping**. If the client doesn't answer with a **pong**
  within `pongWait` (60s), the read deadline fires and we treat it as disconnected. This
  is how we detect a laptop that slept without a clean close.

Outbound messages never touch the socket directly; they go onto the buffered `send`
channel and the write pump delivers them. This decouples "the room wants to broadcast"
from "this particular socket is slow right now."

## Applying and broadcasting an op (the critical section)

`Room.handleOp` is the heart:

```
lock room
  doc.Apply(op)                 // update the authoritative replica
  opsSinceSnapshot++
  broadcast op to local peers   // everyone except the sender
unlock room
  AppendOp(op)                  // Postgres write — SLOW, so outside the lock
  bus.Publish(op)               // Redis fan-out to other pods — outside the lock
  maybe takeSnapshot()          // outside the lock
```

Why this shape?

- **Apply + broadcast are one critical section** so no client can observe a broadcast
  that a concurrent op has already reordered around. The lock keeps the in-memory view
  consistent.
- **Persistence and cross-pod publish are done OUTSIDE the lock.** A disk or network
  write can take milliseconds; holding the room lock across it would serialize the whole
  document's traffic behind one slow write. Because the CRDT tolerates any order, doing
  these after the unlock is safe.

## Backpressure: dropping slow clients (decision D6)

`broadcastLocked` uses a non-blocking send (`enqueue`). If a client's 256-slot buffer is
full, that client can't keep up, so we **disconnect it** rather than block the room. It
reconnects and gets a fresh snapshot — no permanent loss. One slow consumer must never
stall broadcasts to everyone else. This is the standard hub pattern.

## Late joiners: snapshot, not full history

When a client connects, `Room.add` sends it a **snapshot** of the current CRDT plus the
roster. It does _not_ replay the whole operation log to the newcomer. The snapshot is
the compact "here's where we are" that makes joining O(document size) instead of
O(all edits ever). The client loads it with `LoadSnapshotAs(snapshot, myID)` (keeping its
own site id — D5) and is instantly consistent, ready to send and receive ops.

## Rooms come and go

The Hub creates a Room the first time someone opens a document (loading its state from
the store, docs/04) and retires it when the last client leaves (`retireIfEmpty`). If
someone reopens it later, it's recreated and reloaded — no state is lost because the
durable state lives in Postgres, not in the Room. This keeps memory proportional to
_active_ documents, not _all_ documents.

## Multi-instance readiness (the Bus seam)

If you run several server pods behind a load balancer, two people editing one document
might land on different pods. `subscribeBus` + `bus.Publish` bridge them: an op applied
on pod 1 is published to a Redis channel that pod 2 is subscribed to, and pod 2
broadcasts it to its local clients (and applies it to its own replica). Echo suppression
(a pod ignoring its own publishes) lives in the Bus implementation (docs/05). With a nil
Bus, the server is simply single-instance.

## What the tests prove

`server_test.go` runs the real HTTP + WebSocket stack (no DB, no Redis):

- `TestWebSocketRelaysOps` — an op typed by Alice reaches Bob.
- `TestWebSocketConvergenceTwoWay` — Alice and Bob type concurrently into an empty doc
  and **converge to identical text over real sockets**. (This is the test that caught
  the D5 id-collision bug.)
- `TestRoomRetiredWhenEmpty` — the room is cleaned up after the last client leaves.

Run them:

```bash
go test -race ./internal/server -v
```

Next: `docs/04-persistence.md` — giving the server a memory that survives restarts.
