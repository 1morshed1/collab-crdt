package crdt

import (
	"encoding/json"
	"sort"
)

// This file handles turning a Document into bytes and back. Two very different
// consumers need this:
//
//   - Persistence (docs/04): we periodically write a snapshot of the CRDT to
//     Postgres so that loading a document does not require replaying its entire
//     operation history from the beginning of time.
//   - Late joiners (docs/03): when a new client connects to a room, the server
//     ships it a snapshot as the starting point instead of thousands of ops.
//
// We serialize as JSON for readability while learning. A production system would
// use a compact binary encoding, but JSON lets you literally read a snapshot and
// understand it.

// snapNode is the on-the-wire form of a single character. We flatten the tree into
// a list of nodes (each remembering its parent) and rebuild the tree on load. We
// include tombstones — they are part of the CRDT state and must survive a
// save/load round-trip, or deletes could "resurrect".
type snapNode struct {
	ID      CharID `json:"id"`
	Value   string `json:"value"`
	Deleted bool   `json:"deleted"`
	Parent  CharID `json:"parent"`
}

// snapshot is the full serializable state of a replica.
type snapshot struct {
	SiteID  string     `json:"site_id"`
	Lamport uint64     `json:"lamport"`
	Nodes   []snapNode `json:"nodes"`
}

// Snapshot serializes the entire document state (including tombstones and the
// Lamport clock) to bytes.
func (d *Document) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	snap := snapshot{SiteID: d.siteID, Lamport: d.lamport}
	for id, n := range d.nodes {
		if id.IsRoot() {
			continue
		}
		snap.Nodes = append(snap.Nodes, snapNode{
			ID: n.id, Value: n.value, Deleted: n.deleted, Parent: n.parent,
		})
	}
	// Sort for a deterministic, diff-friendly output. Not required for correctness
	// (load re-sorts children), but it makes snapshots stable and easy to inspect.
	sort.Slice(snap.Nodes, func(i, j int) bool {
		return snap.Nodes[i].ID.Less(snap.Nodes[j].ID)
	})
	return json.Marshal(snap)
}

// LoadSnapshot rebuilds a document from a snapshot, keeping the site id stored IN the
// snapshot. This is what the server uses to restore its own replica on restart.
//
// It reuses the same out-of-order buffering logic as live operations, so a snapshot
// whose nodes happen to be listed with a child before its parent still rebuilds
// correctly.
func LoadSnapshot(data []byte) (*Document, error) {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return loadSnapshot(snap, snap.SiteID)
}

// LoadSnapshotAs rebuilds a document from a snapshot but assigns it a DIFFERENT site
// id. This is essential on the client: every browser tab must load the shared document
// state yet keep its OWN unique site id, or the ids it generates would collide with
// another tab that loaded the same snapshot — and colliding ids get silently dropped
// by the idempotency check, so concurrent edits would vanish. (This exact bug is why
// this function exists; see decision D5.)
func LoadSnapshotAs(data []byte, siteID string) (*Document, error) {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return loadSnapshot(snap, siteID)
}

// loadSnapshot is the shared implementation. siteID is applied to the new document
// regardless of what the snapshot stored.
func loadSnapshot(snap snapshot, siteID string) (*Document, error) {
	d := New(siteID)

	// Rebuild by replaying inserts (then applying deletes). applyInsert already
	// handles the case where a parent has not been created yet by parking the child
	// until it can attach, so node ordering in the file does not matter.
	for _, sn := range snap.Nodes {
		d.applyInsert(NewInsert(sn.ID, sn.Parent, sn.Value))
	}
	for _, sn := range snap.Nodes {
		if sn.Deleted {
			d.applyDelete(NewDelete(sn.ID))
		}
	}

	// Restore the clock last: replaying inserts above advanced it via observe(), but
	// we want at least the saved value so id generation continues past everything in
	// the snapshot. (max guards against a caller-supplied site whose replay somehow
	// pushed the clock higher.)
	if snap.Lamport > d.lamport {
		d.lamport = snap.Lamport
	}
	return d, nil
}
