package server

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/1morshed1/collab-crdt/internal/crdt"
)

// snapshotEvery controls how often the server persists a full CRDT snapshot: once
// this many operations have been applied since the last snapshot. Snapshots keep
// document load fast by bounding how many ops must be replayed (docs/04). 1000 is the
// value the original spec suggests.
const snapshotEvery = 1000

// Room is the server's coordinator for a single document. Every client editing that
// document is attached to the same Room. The Room owns the authoritative server-side
// CRDT replica and is the single place where "apply an op, persist it, tell everyone"
// happens atomically.
//
// Locking: r.mu guards the maps and the in-memory bookkeeping (clients, presence,
// counters). The embedded crdt.Document has its own lock, but we still take r.mu
// around apply+broadcast so a client can never observe a broadcast for an op that a
// concurrent op has already reordered around — the two steps stay a single critical
// section. Slow work (database writes, bus publishes) is deliberately done OUTSIDE the
// lock so one slow disk write can't stall the whole room.
type Room struct {
	docID string
	hub   *Hub

	mu       sync.Mutex
	doc      *crdt.Document
	clients  map[string]*Client
	presence map[string]PresenceInfo

	// Persistence bookkeeping (nil store => in-memory only).
	store            Store
	lastSeq          int64 // greatest DB seq this room knows about
	opsSinceSnapshot int

	// Cross-instance fan-out (nil bus => single instance).
	bus        Bus
	busCancel  func()
	instanceID string
}

// newRoom builds a room for docID, loading prior state from the store if one is
// present. It is called by the Hub, which guarantees one Room per docID.
func newRoom(hub *Hub, docID string, store Store, bus Bus, instanceID string) *Room {
	r := &Room{
		docID:      docID,
		hub:        hub,
		doc:        crdt.New("server:" + docID), // server replica's own site id
		clients:    map[string]*Client{},
		presence:   map[string]PresenceInfo{},
		store:      store,
		bus:        bus,
		instanceID: instanceID,
	}
	r.loadFromStore()
	r.subscribeBus()
	return r
}

// loadFromStore reconstructs the document: start from the latest snapshot (if any),
// then replay every operation newer than that snapshot. Because replay is
// order-independent and idempotent, this is robust even if the snapshot boundary is
// loose. See docs/04.
func (r *Room) loadFromStore() {
	if r.store == nil {
		return
	}
	ctx := context.Background()
	if err := r.store.EnsureDocument(ctx, r.docID); err != nil {
		log.Printf("room %s: ensure document: %v", r.docID, err)
		return
	}

	if data, upToSeq, ok, err := r.store.LoadSnapshot(ctx, r.docID); err != nil {
		log.Printf("room %s: load snapshot: %v", r.docID, err)
	} else if ok {
		if doc, err := crdt.LoadSnapshot(data); err != nil {
			log.Printf("room %s: decode snapshot: %v", r.docID, err)
		} else {
			// Keep the server replica's own site id, but adopt the saved structure.
			r.doc = doc
			r.lastSeq = upToSeq
		}
	}

	ops, latestSeq, err := r.store.OpsSince(ctx, r.docID, r.lastSeq)
	if err != nil {
		log.Printf("room %s: load ops: %v", r.docID, err)
		return
	}
	for _, op := range ops {
		r.doc.Apply(op)
	}
	r.lastSeq = latestSeq
	if len(ops) > 0 {
		log.Printf("room %s: loaded snapshot + replayed %d ops (seq<=%d)", r.docID, len(ops), r.lastSeq)
	}
}

// -----------------------------------------------------------------------------
// Client lifecycle.
// -----------------------------------------------------------------------------

// add attaches a client: it ships the client an init message (current document +
// roster) and tells everyone the roster changed.
func (r *Room) add(c *Client) {
	r.mu.Lock()
	snapshot, err := r.doc.Snapshot()
	if err != nil {
		log.Printf("room %s: snapshot for init: %v", r.docID, err)
	}
	roster := r.presenceListLocked() // existing members, before adding c
	r.clients[c.id] = c
	r.presence[c.id] = c.info
	fullRoster := r.presenceListLocked()
	r.mu.Unlock()

	if msg, err := encode(TypeInit, InitData{
		ClientID: c.id,
		Snapshot: json.RawMessage(snapshot),
		Presence: roster,
	}); err == nil {
		c.enqueue(msg)
	}

	r.broadcastPresence(fullRoster)
	log.Printf("room %s: client %s joined (%d online)", r.docID, c.id, len(fullRoster))
}

// remove detaches a client and, if the room emptied, asks the hub to retire it.
// Safe to call more than once for the same client (idempotent).
func (r *Room) remove(c *Client) {
	r.mu.Lock()
	_, present := r.clients[c.id]
	r.dropLocked(c)
	roster := r.presenceListLocked()
	empty := len(r.clients) == 0
	r.mu.Unlock()

	if !present {
		return
	}
	r.broadcastPresence(roster)
	log.Printf("room %s: client %s left (%d online)", r.docID, c.id, len(roster))
	if empty {
		r.hub.retireIfEmpty(r.docID)
	}
}

// dropLocked removes a client from the room and closes its send channel. Closing the
// channel signals the client's writePump to exit. Guarded by r.mu; safe to call while
// ranging over r.clients (Go allows deletion during range).
func (r *Room) dropLocked(c *Client) {
	if _, ok := r.clients[c.id]; !ok {
		return
	}
	delete(r.clients, c.id)
	delete(r.presence, c.id)
	close(c.send)
}

// -----------------------------------------------------------------------------
// Inbound handling.
// -----------------------------------------------------------------------------

// handleOp integrates an operation from a client: apply to the server replica, relay
// to local peers, then (outside the lock) persist and fan out cross-instance.
func (r *Room) handleOp(sender *Client, data json.RawMessage) {
	var op crdt.Operation
	if err := json.Unmarshal(data, &op); err != nil {
		log.Printf("room %s: bad op from %s: %v", r.docID, sender.id, err)
		return
	}

	// Re-encode a canonical op message once, so every recipient gets identical bytes.
	msg, err := encode(TypeOp, op)
	if err != nil {
		return
	}

	r.mu.Lock()
	r.doc.Apply(op)
	r.opsSinceSnapshot++
	needSnapshot := r.store != nil && r.opsSinceSnapshot >= snapshotEvery
	r.broadcastLocked(msg, sender.id)
	r.mu.Unlock()

	// Slow paths outside the lock.
	if r.store != nil {
		if seq, err := r.store.AppendOp(context.Background(), r.docID, op); err != nil {
			log.Printf("room %s: append op: %v", r.docID, err)
		} else {
			r.mu.Lock()
			if seq > r.lastSeq {
				r.lastSeq = seq
			}
			r.mu.Unlock()
		}
	}
	if r.bus != nil {
		if err := r.bus.Publish(context.Background(), r.docID, msg); err != nil {
			log.Printf("room %s: bus publish op: %v", r.docID, err)
		}
	}
	if needSnapshot {
		r.takeSnapshot()
	}
}

// handleCursor records a client's cursor in presence and relays it to peers. Cursor
// updates are high-frequency and ephemeral: we never persist them.
func (r *Room) handleCursor(sender *Client, data json.RawMessage) {
	var cur CursorData
	if err := json.Unmarshal(data, &cur); err != nil {
		return
	}
	cur.ClientID = sender.id // trust the connection, not the payload, for identity

	r.mu.Lock()
	if info, ok := r.presence[sender.id]; ok {
		info.Anchor = cur.Anchor
		info.Head = cur.Head
		r.presence[sender.id] = info
	}
	msg, _ := encode(TypeCursor, cur)
	r.broadcastLocked(msg, sender.id)
	r.mu.Unlock()

	if r.bus != nil {
		_ = r.bus.Publish(context.Background(), r.docID, msg)
	}
}

// -----------------------------------------------------------------------------
// Broadcasting.
// -----------------------------------------------------------------------------

// broadcastLocked sends pre-encoded bytes to every client except exclude. A client
// whose buffer is full is dropped on the spot (its slow consumer must not stall the
// room). Caller holds r.mu.
func (r *Room) broadcastLocked(msg []byte, exclude string) {
	for id, c := range r.clients {
		if id == exclude {
			continue
		}
		if !c.enqueue(msg) {
			log.Printf("room %s: dropping slow client %s", r.docID, id)
			r.dropLocked(c)
		}
	}
}

// broadcastLocalOnly is used for messages arriving from the bus (another instance):
// they must reach this pod's clients but must NOT be re-published to the bus, or two
// pods would echo forever.
func (r *Room) broadcastLocalOnly(msg []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcastLocked(msg, "")
}

// broadcastPresence encodes and sends the roster to all clients.
func (r *Room) broadcastPresence(roster []PresenceInfo) {
	msg, err := encode(TypePresence, PresenceData{Users: roster})
	if err != nil {
		return
	}
	r.mu.Lock()
	r.broadcastLocked(msg, "")
	r.mu.Unlock()
}

// presenceListLocked snapshots the current roster as a slice. Caller holds r.mu.
func (r *Room) presenceListLocked() []PresenceInfo {
	out := make([]PresenceInfo, 0, len(r.presence))
	for _, info := range r.presence {
		out = append(out, info)
	}
	return out
}

// -----------------------------------------------------------------------------
// Cross-instance bus.
// -----------------------------------------------------------------------------

// subscribeBus wires this room to receive messages other instances publish for the
// same document, broadcasting them to local clients.
func (r *Room) subscribeBus() {
	if r.bus == nil {
		return
	}
	ch, cancel, err := r.bus.Subscribe(context.Background(), r.docID)
	if err != nil {
		log.Printf("room %s: bus subscribe: %v", r.docID, err)
		return
	}
	r.busCancel = cancel
	go func() {
		for raw := range ch {
			// A bus message is already an encoded client Message (op or cursor). We
			// also apply ops to our server replica so this pod's authoritative state
			// stays converged even for edits that originated elsewhere.
			var m Message
			if json.Unmarshal(raw, &m) == nil && m.Type == TypeOp {
				var op crdt.Operation
				if json.Unmarshal(m.Data, &op) == nil {
					r.mu.Lock()
					r.doc.Apply(op)
					r.mu.Unlock()
				}
			}
			r.broadcastLocalOnly(raw)
		}
	}()
}

// takeSnapshot serializes the current CRDT and persists it, then resets the counter.
// Runs outside the room lock except for the brief capture of state+seq.
func (r *Room) takeSnapshot() {
	r.mu.Lock()
	data, err := r.doc.Snapshot()
	seq := r.lastSeq
	count := r.opsSinceSnapshot
	r.opsSinceSnapshot = 0
	r.mu.Unlock()
	if err != nil {
		log.Printf("room %s: snapshot encode: %v", r.docID, err)
		return
	}
	if err := r.store.SaveSnapshot(context.Background(), r.docID, data, seq, int64(count)); err != nil {
		log.Printf("room %s: save snapshot: %v", r.docID, err)
		return
	}
	log.Printf("room %s: saved snapshot up to seq %d (%d ops)", r.docID, seq, count)
}

// close releases room resources (called by the hub when retiring an empty room).
func (r *Room) close() {
	if r.busCancel != nil {
		r.busCancel()
	}
}
