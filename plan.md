## Project 10: Real-Time Collaborative Document Editor (CRDTs)

### Concept & Why It Matters

You are building a real-time collaborative text editor where multiple users can edit the same document simultaneously, with changes appearing instantly for all participants. Conflicts are resolved automatically using CRDTs (Conflict-free Replicated Data Types), with no central locking and no operational transformation server.

This project is fundamentally different from everything else on the list. The other projects are about throughput, storage, and infrastructure. This one is about consistency in the face of concurrency — specifically, how to allow concurrent edits without coordination and still converge to the same document state on every client. It is the core problem that Google Docs, Figma, Notion, and every multiplayer tool must solve.

CRDTs are one of the most intellectually demanding topics in distributed systems. Implementing one correctly for text editing (where insert order, delete handling, and cursor positions all interact) demonstrates a level of theoretical understanding that most engineers never reach. It is also a rapidly growing area — every new SaaS product wants "multiplayer" features, and engineers who understand CRDTs are in extremely high demand.

### Architecture Diagram

```
  Browser Client A                            Browser Client B               
  (React + CRDT)                              (React + CRDT)                 
       │                                           │                          
       │  WebSocket                                │  WebSocket               
       │  (send local operations)                  │  (send local operations) 
       ▼                                           ▼                          
┌──────────────────────────────────────────────────────────────────────────────┐
│                        KUBERNETES CLUSTER                                    │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐    │
│  │  Go Collaboration Server                                             │   │
│  │                                                                      │   │
│  │  ┌─────────────────┐    ┌──────────────────────┐                     │   │
│  │  │  WebSocket Hub  │    │  Document Manager    │                     │   │
│  │  │                 │    │                      │                     │   │
│  │  │  - Manages      │    │  - Loads document    │                     │   │
│  │  │    connections   │    │    state from DB     │                     │   │
│  │  │  - Routes ops   │    │  - Applies CRDT ops  │                     │   │
│  │  │    to correct   │    │  - Maintains         │                     │   │
│  │  │    document     │    │    server-side CRDT   │                     │   │
│  │  │    room         │    │    replica            │                     │   │
│  │  │  - Broadcasts   │    │  - Periodic snapshots │                    │   │
│  │  │    to peers     │    │                      │                     │   │
│  │  └─────────────────┘    └──────────────────────┘                     │   │
│  │                                                                      │   │
│  │  ┌─────────────────┐    ┌──────────────────────┐                     │   │
│  │  │  Presence       │    │  Cursor Sync         │                     │   │
│  │  │  Service        │    │  Service             │                     │   │
│  │  │                 │    │                      │                     │   │
│  │  │  - Who is       │    │  - Tracks each       │                     │   │
│  │  │    online       │    │    user's cursor     │                     │   │
│  │  │  - User colors  │    │    and selection     │                     │   │
│  │  │  - Typing       │    │  - Translates cursor │                     │   │
│  │  │    indicators   │    │    positions across  │                     │   │
│  │  │                 │    │    CRDT operations   │                     │   │
│  │  └─────────────────┘    └──────────────────────┘                     │   │
│  └──────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│  ┌──────────────────────────────────────────────────┐                        │
│  │  PostgreSQL                                      │                       │
│  │                                                  │                       │
│  │  documents (id, title, owner_id, created_at)     │                       │
│  │  document_snapshots (doc_id, snapshot_data,      │                       │
│  │                       version, created_at)       │                       │
│  │  operations_log (doc_id, operation, client_id,   │                       │
│  │                  lamport_ts, created_at)          │                       │
│  │  collaborators (doc_id, user_id, permission)     │                       │
│  └──────────────────────────────────────────────────┘                        │
│                                                                              │
│  ┌──────────────────────────────────────────────────┐                        │
│  │  Redis                                           │                       │
│  │                                                  │                       │
│  │  - Active sessions per document                  │                       │
│  │  - Presence data (user online status)            │                       │
│  │  - Pub/Sub for multi-instance sync               │                       │
│  │    (if running multiple server pods)              │                       │
│  └──────────────────────────────────────────────────┘                        │
└──────────────────────────────────────────────────────────────────────────────┘
```

### The CRDT: Why and How

**Why not Operational Transformation (OT)?**

Google Docs uses OT, which requires a central server to order and transform operations. OT is correct but has a fundamental limitation: every operation must pass through the server before it can be applied. This adds latency and creates a single point of failure.

CRDTs take a different approach: operations are designed so that they can be applied in any order and will always converge to the same result. No central ordering needed. Each client applies operations locally (instant feedback), broadcasts them to peers, and peers apply them whenever they arrive. The mathematical properties of the CRDT guarantee convergence.

**The Algorithm: RGA (Replicated Growable Array)**

RGA is one of the most well-understood sequence CRDTs. Each character in the document is assigned a unique identifier that determines its position.

```go
// Each character in the document
type Char struct {
    ID        CharID
    Value     rune       // the actual character, or a tombstone marker
    Deleted   bool       // tombstone: logically deleted but still present in structure
    ParentID  CharID     // the character this was inserted after
}

// Globally unique, totally ordered identifier
type CharID struct {
    Lamport   uint64     // Lamport timestamp (logical clock)
    SiteID    string     // unique client/replica identifier
}

// CharIDs are compared lexicographically: Lamport first, then SiteID
func (a CharID) LessThan(b CharID) bool {
    if a.Lamport != b.Lamport {
        return a.Lamport < b.Lamport
    }
    return a.SiteID < b.SiteID
}
```

**How Insert Works:**

When User A types "H" after the document root:
1. User A's client generates a new `CharID{Lamport: 1, SiteID: "A"}`.
2. The operation is: `Insert{Char: 'H', ID: {1, "A"}, ParentID: ROOT}`.
3. Applied locally immediately (User A sees "H" on screen).
4. Broadcast to all peers via WebSocket.

When User B simultaneously types "X" after the same root:
1. User B generates `CharID{Lamport: 1, SiteID: "B"}`.
2. The operation is: `Insert{Char: 'X', ID: {1, "B"}, ParentID: ROOT}`.
3. Applied locally (User B sees "X").
4. Broadcast to peers.

When both operations arrive at a third replica (or at the server):
- Both "H" and "X" have the same parent (ROOT).
- The CRDT resolves the conflict using the total order on CharIDs: `{1, "A"} < {1, "B"}` because "A" < "B".
- Result: "HX" on every replica. Always. Regardless of arrival order.

```go
type Document struct {
    chars   []Char           // the linear sequence of characters (including tombstones)
    index   map[CharID]int   // CharID -> position in chars slice
    lamport uint64           // this replica's logical clock
    siteID  string
}

func (d *Document) Insert(op InsertOp) {
    // Update Lamport clock
    if op.ID.Lamport > d.lamport {
        d.lamport = op.ID.Lamport
    }
    d.lamport++

    // Find the parent's position
    parentPos := d.index[op.ParentID]

    // Find the correct insertion position among siblings
    // (characters that share the same parent)
    insertPos := parentPos + 1
    for insertPos < len(d.chars) {
        sibling := d.chars[insertPos]
        
        // If this sibling has a different parent, we've passed all siblings
        if sibling.ParentID != op.ParentID {
            // Check if this sibling is a descendant of a sibling we already passed
            // (it could be a character inserted after one of our siblings)
            if !d.isDescendantOfRange(sibling, parentPos+1, insertPos) {
                break
            }
        }
        
        // Among siblings with the same parent, higher CharID goes first (leftmost)
        if sibling.ParentID == op.ParentID && op.ID.LessThan(sibling.ID) {
            insertPos++
            continue
        }
        
        break
    }

    // Insert the character
    newChar := Char{ID: op.ID, Value: op.Value, ParentID: op.ParentID}
    d.chars = slices.Insert(d.chars, insertPos, newChar)
    d.rebuildIndex()
}

func (d *Document) Delete(op DeleteOp) {
    pos := d.index[op.TargetID]
    d.chars[pos].Deleted = true  // tombstone, do not remove from the array
}

func (d *Document) Text() string {
    var buf strings.Builder
    for _, c := range d.chars {
        if !c.Deleted {
            buf.WriteRune(c.Value)
        }
    }
    return buf.String()
}
```

**Why Tombstones?**

When a character is deleted, you cannot remove it from the array. Other operations may reference it as a parent. If you removed it, those operations would fail to find their insertion point. Instead, mark it as deleted (a "tombstone") and skip it when rendering the visible text.

Over time, tombstones accumulate. To manage this, periodically "garbage collect" tombstones that are older than all known operations (i.e., every replica has seen them). This is the snapshot/compaction mechanism.

#### Phase 2: The Collaboration Server (Go + WebSocket)

**WebSocket Message Types:**

```go
type WSMessage struct {
    Type string          `json:"type"`
    Data json.RawMessage `json:"data"`
}

// Possible types:
// "insert"    - InsertOp: a character was inserted
// "delete"    - DeleteOp: a character was deleted
// "cursor"    - CursorUpdate: a user's cursor moved
// "presence"  - PresenceUpdate: a user joined/left
// "sync"      - SyncRequest/SyncResponse: initial state sync
// "snapshot"  - Full document snapshot (for late joiners)
```

**Room Management:**

Each document is a "room." When a user connects:

1. Add them to the room's connection list.
2. Send the latest document snapshot (or the full operation log if no snapshot exists).
3. Send the current presence list (who else is editing, their cursor positions, their assigned colors).
4. Start relaying operations bidirectionally.

```go
type Room struct {
    docID       string
    clients     map[string]*Client     // client_id -> websocket connection
    document    *Document              // server-side CRDT replica
    mu          sync.RWMutex
    opsBuffer   []Operation            // operations since last snapshot
}

func (r *Room) HandleOperation(clientID string, op Operation) {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    // Apply to server-side replica
    r.document.Apply(op)
    r.opsBuffer = append(r.opsBuffer, op)
    
    // Persist to operations log
    r.persistOp(op)
    
    // Broadcast to all other clients in the room
    for id, client := range r.clients {
        if id != clientID {
            client.Send(op)
        }
    }
    
    // Snapshot every 1000 operations
    if len(r.opsBuffer) >= 1000 {
        r.takeSnapshot()
    }
}
```

**Multi-Instance Scaling with Redis Pub/Sub:**

If you run multiple server pods behind a load balancer, two clients editing the same document might connect to different pods. To keep them in sync, use Redis Pub/Sub:

```go
func (r *Room) HandleOperation(clientID string, op Operation) {
    // ... apply locally and broadcast to local clients ...
    
    // Also publish to Redis so other server instances receive it
    r.redis.Publish(ctx, "doc:"+r.docID, serializeOp(op))
}

// Each server instance subscribes to its active documents
func (s *Server) subscribeToDocument(docID string) {
    sub := s.redis.Subscribe(ctx, "doc:"+docID)
    for msg := range sub.Channel() {
        op := deserializeOp(msg.Payload)
        room := s.getRoom(docID)
        room.ApplyRemoteOp(op)  // apply + broadcast to local clients
    }
}
```

#### Phase 3: Cursor and Presence Synchronization

**Cursor Positions in a CRDT World:**

Cursor positions cannot be represented as simple integer offsets because offsets change as other users insert or delete characters. Instead, represent cursor positions as CharIDs — the cursor is "after character with ID {Lamport: 5, SiteID: 'A'}." This position is stable regardless of concurrent insertions.

```go
type CursorPosition struct {
    AnchorID CharID  // the character the cursor is positioned after
    HeadID   CharID  // for selections: the other end of the selection
    UserID   string
    Color    string  // assigned color for this user's cursor
}

// Convert CharID-based cursor to a screen offset for rendering
func (d *Document) CursorToOffset(charID CharID) int {
    offset := 0
    for _, c := range d.chars {
        if c.ID == charID {
            return offset
        }
        if !c.Deleted {
            offset++
        }
    }
    return offset
}
```

**Presence:**

Presence is simpler than the CRDT — it is ephemeral state. Use Redis to track who is online in each document. When a user connects, add them to a Redis set. When they disconnect (or their heartbeat times out), remove them. Broadcast presence changes to all clients in the room.

```go
type PresenceData struct {
    UserID    string `json:"user_id"`
    Name      string `json:"name"`
    Color     string `json:"color"`
    Cursor    CursorPosition `json:"cursor"`
    IsTyping  bool   `json:"is_typing"`
}
```

#### Phase 4: Persistence and Recovery

**Operation Log:**

Every operation is persisted to the `operations_log` table. This provides a complete history of every edit, enabling undo, version history, and recovery from server crashes.

```sql
CREATE TABLE operations_log (
    id              BIGSERIAL PRIMARY KEY,
    document_id     UUID NOT NULL REFERENCES documents(id),
    operation_type  VARCHAR(10) NOT NULL,       -- 'insert', 'delete'
    operation_data  JSONB NOT NULL,              -- the full operation
    client_id       TEXT NOT NULL,
    lamport_ts      BIGINT NOT NULL,
    server_ts       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_ops_doc_lamport ON operations_log (document_id, lamport_ts ASC);
```

**Snapshots:**

Replaying thousands of operations on every document load is slow. Periodically snapshot the CRDT state:

```sql
CREATE TABLE document_snapshots (
    id              BIGSERIAL PRIMARY KEY,
    document_id     UUID NOT NULL REFERENCES documents(id),
    crdt_state      BYTEA NOT NULL,              -- serialized CRDT structure
    lamport_ts      BIGINT NOT NULL,             -- the Lamport timestamp at snapshot time
    operation_count BIGINT NOT NULL,             -- how many ops are baked into this snapshot
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

On load: fetch the latest snapshot, then replay only operations with `lamport_ts > snapshot.lamport_ts`.

#### Phase 5: Version History and Undo

Because you have the full operation log, you can reconstruct the document at any point in time:

```go
func (s *Store) GetDocumentAtTime(ctx context.Context, docID uuid.UUID, at time.Time) *Document {
    // Find the latest snapshot before `at`
    snapshot := s.getLatestSnapshotBefore(ctx, docID, at)
    
    // Replay operations between snapshot and target time
    ops := s.getOperationsBetween(ctx, docID, snapshot.LamportTS, at)
    
    doc := deserializeDocument(snapshot.CRDTState)
    for _, op := range ops {
        doc.Apply(op)
    }
    return doc
}
```

This enables a "version history" slider in the UI — drag it to see the document at any point in its editing history, exactly like Google Docs version history.

### Load Testing

- **Convergence test:** Run 5 concurrent clients each inserting 1,000 characters into random positions simultaneously. Verify all 5 end up with identical document state. This is the correctness test — if it fails, the CRDT implementation has a bug.
- **Latency:** Measure time from one client's keystroke to the character appearing on another client's screen. Target: under 50ms on localhost, under 200ms over the internet.
- **Scale:** How many concurrent editors can one document support before the WebSocket broadcast becomes a bottleneck? Target: 50+ concurrent editors.
- **Document size:** Test with documents of 1,000, 10,000, and 100,000 characters. Measure operation application time as the document grows.

### Cost Management

Zero cloud cost. Everything runs locally. The server is a single Go binary. Redis and PostgreSQL in Docker Compose. The client is a React app served by the Go server. Open two browser tabs to simulate collaboration.

---
---