package crdt

import (
	"sort"
	"sync"
)

// node is one character's home in the RGA tree. We model the document as a tree
// (not a flat array) because it makes the integration rule — "insert after X" —
// trivially correct: it just means "attach as a child of X". The visible text is a
// depth-first, pre-order walk of this tree. See docs/01 and decision D3.
type node struct {
	id       CharID
	value    string
	deleted  bool    // tombstone flag; true means "logically deleted, still present"
	parent   CharID  // the character this was inserted after (Root for start-of-doc)
	children []*node // kept sorted by id DESCENDING (higher id first) — see D2
}

// Document is a single RGA replica. Each browser tab has one, and the server has
// one per open document ("room"). Every replica that has seen the same set of
// operations holds byte-identical text — that is the guarantee we test to death in
// convergence_test.go.
//
// Concurrency: Document guards its own state with a mutex, so it is safe to call
// from multiple goroutines. The server's Room still takes its own lock around
// "apply then broadcast" to keep those two steps atomic, but Document does not rely
// on that for its own integrity.
type Document struct {
	mu sync.Mutex

	siteID  string           // this replica's identity, used when it generates ops
	lamport uint64           // this replica's logical clock (see docs/01)
	root    *node            // the invisible root sentinel
	nodes   map[CharID]*node // fast lookup: id -> node (includes tombstones)

	// Out-of-order buffers. A CRDT must converge no matter what ORDER ops arrive
	// in, including a child insert arriving before its parent, or a delete arriving
	// before the character it targets. Rather than requiring the network to deliver
	// ops causally, we simply park an op that references a not-yet-known character
	// and replay it the moment that character shows up. This is what turns "usually
	// converges" into "always converges". See docs/01 on causal readiness.
	pendingChildren map[CharID][]Operation // parent id -> inserts waiting for it
	pendingDeletes  map[CharID]bool        // target id -> a delete waiting for it
}

// New creates an empty document replica identified by siteID. The siteID must be
// unique per replica (per browser tab, per server instance) — it is the tiebreaker
// that makes concurrent edits deterministic.
func New(siteID string) *Document {
	root := &node{id: Root}
	return &Document{
		siteID:          siteID,
		root:            root,
		nodes:           map[CharID]*node{Root: root},
		pendingChildren: map[CharID][]Operation{},
		pendingDeletes:  map[CharID]bool{},
	}
}

// SiteID returns this replica's identity.
func (d *Document) SiteID() string { return d.siteID }

// -----------------------------------------------------------------------------
// Local editing: these generate NEW operations (assigning fresh CharIDs) and apply
// them to this replica. The returned Operation is what you broadcast to peers.
// -----------------------------------------------------------------------------

// LocalInsert inserts value after the character identified by after (use Root for
// the start of the document), assigning it a fresh CharID from this replica's
// clock. It returns the operation to broadcast.
func (d *Document) LocalInsert(after CharID, value string) Operation {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lamport++
	op := NewInsert(CharID{Lamport: d.lamport, Site: d.siteID}, after, value)
	d.applyLocked(op)
	return op
}

// LocalDelete tombstones the character identified by target and returns the
// operation to broadcast.
func (d *Document) LocalDelete(target CharID) Operation {
	d.mu.Lock()
	defer d.mu.Unlock()
	op := NewDelete(target)
	d.applyLocked(op)
	return op
}

// -----------------------------------------------------------------------------
// Applying operations (local or remote). Apply is idempotent and order-independent:
// applying the same op twice is a no-op, and applying ops in any order converges.
// Those two properties are the whole ballgame — see docs/00 on SEC.
// -----------------------------------------------------------------------------

// Apply integrates a remote operation into this replica.
func (d *Document) Apply(op Operation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applyLocked(op)
}

// applyLocked does the real work and assumes d.mu is held.
func (d *Document) applyLocked(op Operation) {
	switch op.Type {
	case OpInsert:
		d.observe(op.ID.Lamport) // keep our clock ahead of anything we've seen
		d.applyInsert(op)
	case OpDelete:
		d.applyDelete(op)
	}
}

// observe advances the Lamport clock so that any id we generate later is greater
// than any id we have ever seen. This is the receive half of a Lamport clock.
func (d *Document) observe(ts uint64) {
	if ts > d.lamport {
		d.lamport = ts
	}
}

func (d *Document) applyInsert(op Operation) {
	// Idempotency: if we already have this character, ignore. This is what makes
	// duplicate delivery harmless.
	if _, exists := d.nodes[op.ID]; exists {
		return
	}

	parent, ok := d.nodes[op.After]
	if !ok {
		// The parent hasn't arrived yet. Park this op and replay it when the parent
		// shows up. Do NOT drop it — dropping would break convergence.
		d.pendingChildren[op.After] = append(d.pendingChildren[op.After], op)
		return
	}

	n := &node{id: op.ID, value: op.Value, parent: op.After}

	// If a delete for this exact character arrived before the character itself,
	// honor it now.
	if d.pendingDeletes[op.ID] {
		n.deleted = true
		delete(d.pendingDeletes, op.ID)
	}

	d.nodes[op.ID] = n
	insertChildSorted(parent, n)

	// Now that op.ID exists, any inserts that were waiting for it as a parent can
	// proceed. We loop because flushing may itself unblock deeper descendants.
	if waiting, ok := d.pendingChildren[op.ID]; ok {
		delete(d.pendingChildren, op.ID)
		for _, w := range waiting {
			d.applyInsert(w)
		}
	}
}

func (d *Document) applyDelete(op Operation) {
	n, ok := d.nodes[op.Target]
	if !ok {
		// Delete arrived before the character it targets. Remember it; applyInsert
		// will honor it when the character appears.
		d.pendingDeletes[op.Target] = true
		return
	}
	n.deleted = true // tombstone. We never unlink the node — see docs/01.
}

// insertChildSorted places n among parent's children keeping them sorted by id
// DESCENDING (higher id first). This single rule is how concurrent inserts at the
// same location get a deterministic, identical order on every replica.
func insertChildSorted(parent, n *node) {
	// Find the first position whose child id is LESS than n.id; insert there.
	i := sort.Search(len(parent.children), func(i int) bool {
		return parent.children[i].id.Less(n.id)
	})
	parent.children = append(parent.children, nil)
	copy(parent.children[i+1:], parent.children[i:])
	parent.children[i] = n
}

// -----------------------------------------------------------------------------
// Reading the document.
// -----------------------------------------------------------------------------

// Text returns the current visible text: a pre-order DFS of the tree, skipping
// tombstoned characters.
func (d *Document) Text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b []rune
	d.walk(d.root, func(n *node) {
		if !n.deleted {
			b = append(b, []rune(n.value)...)
		}
	})
	return string(b)
}

// walk performs a pre-order depth-first traversal: visit a node, then its children
// in order (which are sorted by id descending). The root itself carries no
// character, so callers skip it via the deleted/value checks.
func (d *Document) walk(n *node, visit func(*node)) {
	if !n.id.IsRoot() {
		visit(n)
	}
	for _, c := range n.children {
		d.walk(c, visit)
	}
}

// visibleNodes returns, in document order, the ids of all non-deleted characters.
// Used for cursor<->offset conversion. Caller must hold d.mu.
func (d *Document) visibleNodesLocked() []*node {
	var out []*node
	d.walk(d.root, func(n *node) {
		if !n.deleted {
			out = append(out, n)
		}
	})
	return out
}

// Len returns the number of visible (non-deleted) characters.
func (d *Document) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.visibleNodesLocked())
}

// -----------------------------------------------------------------------------
// Cursor <-> offset conversion.
//
// The editor UI thinks in integer offsets ("cursor is between the 4th and 5th
// character"). The CRDT thinks in stable CharIDs. These helpers translate between
// the two. The key insight (docs/02): a cursor is stored as "after character with
// id X"; that anchor stays meaningful even as other people insert and delete around
// it, whereas a raw integer offset would silently point at the wrong place.
// -----------------------------------------------------------------------------

// OffsetToAfterID maps a visible cursor offset (0 = start of document) to the
// CharID the cursor sits immediately after. Insert at that anchor to land the new
// character exactly at the offset. Out-of-range offsets are clamped.
func (d *Document) OffsetToAfterID(offset int) CharID {
	d.mu.Lock()
	defer d.mu.Unlock()
	vis := d.visibleNodesLocked()
	if offset <= 0 {
		return Root
	}
	if offset > len(vis) {
		offset = len(vis)
	}
	return vis[offset-1].id
}

// AfterIDToOffset is the inverse: given an anchor CharID, return the visible offset
// just after it. Root maps to 0. An anchor that is currently tombstoned or unknown
// maps to the offset of the nearest visible character at or before it, which keeps
// a remote cursor sensible even if the character it was anchored to just got
// deleted.
func (d *Document) AfterIDToOffset(after CharID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if after.IsRoot() {
		return 0
	}
	// Walk the full document (including tombstones) counting visible characters
	// until we pass the anchor.
	offset := 0
	found := 0
	d.walk(d.root, func(n *node) {
		if n.id.Equal(after) {
			found = offset
			if !n.deleted {
				found = offset + 1
			}
		}
		if !n.deleted {
			offset++
		}
	})
	return found
}

// TargetAtOffset returns the CharID of the visible character at index i (0-based),
// i.e. the character you would delete for a backspace/delete at that spot. ok is
// false if i is out of range.
func (d *Document) TargetAtOffset(i int) (CharID, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vis := d.visibleNodesLocked()
	if i < 0 || i >= len(vis) {
		return CharID{}, false
	}
	return vis[i].id, true
}
