# 04 — Persistence: an operation log and snapshots

> Code: `internal/store/*.go`, `internal/store/migrations/0001_init.sql`.
> Tests: `internal/store/store_test.go` (integration; need a real Postgres).

So far the server keeps everything in memory. Restart it and every document is gone.
This phase gives it a durable memory in PostgreSQL, designed so that documents both
**survive restarts** and **load quickly** even after millions of edits.

## Two ideas, working together

1. **The operation log** — an append-only table (`operations_log`) that records every
   single edit, forever. This is the source of truth and the raw material for version
   history.
2. **Snapshots** — periodic, serialized full CRDT states (`document_snapshots`). A
   snapshot is a shortcut: "here is exactly what the document looked like as of edit N,
   so you don't have to replay edits 1..N to find out."

Loading a document = **load the latest snapshot, then replay only the operations newer
than it.** That's O(document size + recent edits), not O(all edits ever). See
`Room.loadFromStore` in `internal/server/room.go` and `TestFullReloadPath`.

## The schema

```sql
documents(id, title, created_at)
operations_log(seq BIGSERIAL, document_id, op_type, op_data JSONB, created_at)
document_snapshots(id, document_id, crdt_state BYTEA, up_to_seq, op_count, created_at)
```

- `op_data` is the whole `crdt.Operation` as JSON — the exact same struct the WebSocket
  carries. One representation, everywhere.
- `crdt_state` is the bytes from `crdt.Document.Snapshot()`.
- Note there is **no auth / collaborators table**. This project has no login layer, so a
  document id from the URL is all we track. A real app would add users, permissions, and
  UUID document ids (see the commented notes in the migration).

## The one subtle decision: `seq`, not Lamport, marks the boundary

You might expect the snapshot boundary to be a Lamport timestamp ("this snapshot includes
everything up to Lamport 5000"). **It can't be**, and understanding why is the whole
lesson of this doc.

A **delete** operation tombstones a character that may be very old. Its effect happens
"now," but the character it targets has a small Lamport value. If we filtered "ops since
the snapshot" by Lamport, a fresh delete of an ancient character would be assigned a
small effective timestamp and get **excluded** from the replay — so on reload, the delete
would be lost and the character would resurrect.

The fix is to use the database's own monotonic `BIGSERIAL seq`, which strictly increases
with insertion order regardless of what the op does or how old its target is. Every op
gets the next `seq`; snapshots record `up_to_seq`; reload replays `seq > up_to_seq`. Both
inserts and deletes are handled uniformly. This is exactly what the `Store` interface
promises and why `AppendOp` returns the assigned seq. (See the extended comment in
`interfaces.go`.)

Because CRDT replay is **idempotent** (doc 01), the boundary doesn't even have to be
exact — if a couple of already-snapshotted ops get replayed again, `Apply` dedupes them.
That slack is what lets `Room.takeSnapshot` capture "state + current seq" without an
expensive global lock.

## Writing: append on the hot path, snapshot occasionally

- `AppendOp` runs on **every** edit, but **outside the room lock** (docs/03) so a slow
  disk never stalls live collaboration.
- `takeSnapshot` runs every `snapshotEvery` (1000) ops. It serializes the current CRDT
  and writes one row, then prunes older snapshots for that document — we only ever need
  the newest one to load. The op log is **kept**, because version history needs it.

## Reading back: the exact reload path

`Room.loadFromStore` (server package) does what `TestFullReloadPath` verifies:

```
EnsureDocument(docID)                       // create the row if new
data, upToSeq, ok = LoadSnapshot(docID)     // newest snapshot, if any
if ok: doc = crdt.LoadSnapshot(data)        // start from it
ops, latest = OpsSince(docID, upToSeq)      // the tail newer than the snapshot
for op in ops: doc.Apply(op)                // replay (order-independent, idempotent)
lastSeq = latest
```

A brand-new document has no snapshot and no ops, so it starts empty. A document edited a
million times loads a ~current-size snapshot plus at most ~1000 tail ops.

## Migrations that ship inside the binary

`migrate.go` embeds the `.sql` files with `//go:embed` and applies any that haven't run
yet (tracked in a `schema_migrations` table), each in its own transaction. So a freshly
built server against an empty database just works — no external migration tool, no files
to deploy. To evolve the schema, add `0002_*.sql` and it runs on the next boot. (The
files live under the `store` package because `go:embed` can't reach parent directories.)

## Version history & time-travel (why we keep the op log)

Because every op is retained with a `created_at` and a `seq`, you can reconstruct the
document as of any past moment: load the latest snapshot **before** that moment, then
replay ops up to it — exactly the `GetDocumentAtTime` sketch in the original spec. That powers a
"drag the slider to see the doc last Tuesday" feature like Google Docs history. The
building blocks (`LoadSnapshot`, `OpsSince`) are already here; wiring a UI slider is left
as a documented extension.

## What about tombstones growing forever?

Snapshots bound how many *ops* you replay, but the CRDT state itself still carries every
tombstone (doc 01 §5). True reclamation — **garbage-collecting tombstones** once every
replica has seen the delete — is a distinct mechanism touched on in docs/05. At this
project's realistic document sizes it's not a problem; we call it out honestly rather
than pretend it away.

## Running the tests

```bash
docker compose up -d db     # or the standalone container in docs/07
TEST_DATABASE_URL=postgres://collab:collab@localhost:5432/collab go test ./internal/store -v
```

Without `TEST_DATABASE_URL` these tests skip, so `go test ./...` stays green with no
database installed.

Next: `docs/05-presence-and-redis.md` — who's online, live cursors, and scaling to
multiple server pods.
