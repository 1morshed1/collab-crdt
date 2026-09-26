# 05 — Presence, live cursors, and scaling out with Redis

> Code: `internal/presence/bus.go`. Tests: `internal/presence/bus_test.go`
> (bus unit tests) and `internal/server/multipod_test.go` (two pods via Redis).

This phase covers two related things: **presence** (who's here, where their cursors
are) and **horizontal scaling** (running more than one server process). Redis is the
tool for both, but they use it in different ways, and it's worth being precise about
which is which.

## Presence is ephemeral — the opposite of the CRDT

The CRDT state (docs 01/04) is *durable and convergent*: every edit matters forever and
must survive crashes. Presence is the opposite — it is **ephemeral**:

- Who is online right now.
- Each person's name and assigned color.
- Where each person's caret and selection are (as **CharIDs**, so they stay meaningful
  as text shifts — docs/02).
- (Optionally) typing indicators.

None of this is worth persisting. If the server restarts, presence is simply rebuilt as
clients reconnect. So presence never touches Postgres. Within a single pod it lives in
the Room's in-memory `presence` map (`internal/server/room.go`), broadcast to clients as
`presence` (the roster) and `cursor` (live caret) messages.

### Cursors ride the same broadcast path as ops

A `cursor` message is just another envelope. `Room.handleCursor` updates the sender's
presence entry and relays the message to peers — and, when a bus is present, publishes it
across pods too. So remote carets move live even across server instances. Cursor updates
are high-frequency and disposable; we never store them and never replay them.

## Scaling out: why one process isn't enough forever

A single Go process can handle a lot of WebSockets, but eventually you want several — for
CPU, for resilience, for rolling deploys. Put them behind a load balancer and a problem
appears immediately:

```
  Alice ──▶ Pod A ──┐            Pod B ──▶ Bob
                    │  Alice's op is broadcast only to Pod A's clients.
                    ✗  Bob, on Pod B, never sees it.
```

Pod A's in-memory broadcast never leaves Pod A. Alice and Bob diverge — not because the
CRDT is wrong, but because the op never reached Bob's replica.

## Redis Pub/Sub bridges the pods (the Bus)

The fix (straight from `plan.md`'s scaling section): every pod **publishes** each
document's messages to a Redis channel, and every pod **subscribes** to the channels for
documents it has clients for.

```
  Alice ─▶ Pod A ─▶ redis PUBLISH collab:doc:notes ─┐
                                                     ├─▶ Pod A (ignores own echo)
                                                     └─▶ Pod B ─▶ Bob
```

`RedisBus` (in `bus.go`) implements the `server.Bus` interface with three responsibilities:

1. **Publish** — wrap the message in an `envelope{origin, payload}` and `PUBLISH` it to
   `collab:doc:<id>`.
2. **Subscribe** — `SUBSCRIBE` to the channel, and for each message, unwrap it and
   forward the payload to the Room, which broadcasts it to that pod's local clients.
3. **Echo suppression** — Redis delivers a publish to *every* subscriber, including the
   publisher. The `origin` field carries the publishing pod's instance id; a pod drops
   messages whose origin is itself. Without this, a pod would double-broadcast its own
   ops and two pods could echo forever.

### Why this is safe: the CRDT does the hard part

Notice the Bus makes **no ordering guarantees** across pods. Redis Pub/Sub is
fire-and-forget; messages can arrive in different orders on different pods, and a pod that
wasn't subscribed yet misses a message entirely. That would be catastrophic for a naive
"apply insert at index" system. It's fine here because:

- The CRDT is **order-independent and idempotent** (docs/01), so out-of-order or
  duplicated cross-pod delivery still converges.
- Any client that fell behind or connected late gets a fresh **snapshot** on connect
  (docs/03), and every op is durably in Postgres (docs/04), so a briefly-missed Pub/Sub
  message is recoverable.

The dumb, lossy transport is acceptable precisely because the data type is robust. That
is the recurring theme of this whole project.

## What the tests prove

- `bus_test.go` (needs `TEST_REDIS_ADDR`): cross-instance delivery works, a pod does
  **not** receive its own echoes, and channels are isolated per document.
- `multipod_test.go` (needs `TEST_REDIS_ADDR`): the real end-to-end win — two separate
  HTTP servers with two Hubs and two instance ids, sharing one Redis. A client on **pod
  A** types; a client connected to **pod B** receives the text. This is the property that
  the whole phase exists to deliver.

```bash
docker compose up -d redis
TEST_REDIS_ADDR=localhost:6379 go test ./internal/presence -v
TEST_REDIS_ADDR=localhost:6379 go test ./internal/server -run TestTwoPods -v
```

## Honest limitations (and the path to fixing them)

Being a learning repo, here's exactly where the current implementation stops and what a
production system adds. See decision **D7**.

1. **The roster (who's online) is currently per-pod.** Ops and live cursors cross pods
   via the Bus, so editing and remote carets work cluster-wide. But each pod's `presence`
   roster only lists *its own* clients, so the "N people here" list can undercount when
   editors are spread across pods. The standard fix: store presence in a **Redis set per
   document** with a **TTL heartbeat** (each client refreshes its entry every few
   seconds; a crashed pod's entries expire automatically), and publish roster changes so
   every pod renders the union. The Bus and cursor plumbing are already in place; this is
   an additive change, called out rather than hidden.

2. **Tombstone garbage collection isn't implemented.** Snapshots bound op *replay*
   (docs/04) but the CRDT keeps every tombstone. Reclaiming them requires knowing every
   replica has seen a delete — typically via a version vector of acknowledged ops across
   pods (which Redis can coordinate). At the sizes this project targets it's a
   non-issue; a production system at scale would add it.

Next: `docs/06-frontend.md` — the browser client that turns all of this into an editor
you can actually type in.
