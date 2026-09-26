-- 0001_init.sql — the initial schema.
--
-- Design notes (the "why", expanded in docs/04):
--
--   * document ids are plain TEXT. In this project the id comes straight from the
--     URL (?doc=notes). A production system with auth would use UUIDs and a
--     collaborators table; we keep it simple since there is no auth layer here.
--
--   * operations_log is append-only and is the source of truth for history. Its
--     BIGSERIAL `seq` is the monotonic boundary marker the server uses to tell
--     "what's already in a snapshot" from "what's newer" (see docs/04 and the Store
--     interface). We deliberately do NOT order replay by the CRDT's Lamport clock,
--     because a delete of an old character would carry a small effective timestamp.
--
--   * document_snapshots stores a serialized CRDT state so loading a document is
--     O(snapshot + recent ops) instead of O(all history). We keep the op log even
--     after snapshotting so version history / time-travel stays possible (docs/04).

CREATE TABLE IF NOT EXISTS documents (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL DEFAULT 'Untitled',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS operations_log (
    seq         BIGSERIAL PRIMARY KEY,
    document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    op_type     TEXT NOT NULL,               -- 'insert' | 'delete'
    op_data     JSONB NOT NULL,              -- the full crdt.Operation as JSON
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The workhorse index: "give me every op for this document after seq N, in order."
CREATE INDEX IF NOT EXISTS idx_ops_doc_seq ON operations_log (document_id, seq);

CREATE TABLE IF NOT EXISTS document_snapshots (
    id          BIGSERIAL PRIMARY KEY,
    document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    crdt_state  BYTEA NOT NULL,              -- serialized crdt.Document (see Snapshot)
    up_to_seq   BIGINT NOT NULL,             -- ops with seq <= this are baked in
    op_count    BIGINT NOT NULL,             -- how many ops since the previous snapshot
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "Give me the newest snapshot for this document."
CREATE INDEX IF NOT EXISTS idx_snap_doc_seq ON document_snapshots (document_id, up_to_seq DESC);
