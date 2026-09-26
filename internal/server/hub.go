package server

import (
	"sync"
)

// Hub owns all the rooms in one server process and maps a document id to its Room.
// It is the traffic controller: when a client connects for document X, the hub either
// hands back the existing Room for X or creates one (loading X's state from the store).
//
// Why a hub at all? Because many clients editing the same document must share ONE
// server-side replica and ONE broadcast set. The hub guarantees that "one Room per
// document" invariant with a single lock around the map.
type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room

	store      Store
	bus        Bus
	instanceID string
}

// NewHub creates a hub. store and bus may be nil (single-instance, in-memory mode).
// instanceID identifies this server process for cross-instance echo suppression.
func NewHub(store Store, bus Bus, instanceID string) *Hub {
	return &Hub{
		rooms:      map[string]*Room{},
		store:      store,
		bus:        bus,
		instanceID: instanceID,
	}
}

// room returns the Room for docID, creating (and loading) it on first use.
func (h *Hub) room(docID string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[docID]; ok {
		return r
	}
	r := newRoom(h, docID, h.store, h.bus, h.instanceID)
	h.rooms[docID] = r
	return r
}

// retireIfEmpty removes a room once its last client has left, freeing its resources
// (and its bus subscription). If a new client arrives later, the room is recreated and
// reloaded from the store, so no state is lost. We re-check emptiness under the lock to
// avoid racing with a client that connected in the meantime.
func (h *Hub) retireIfEmpty(docID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[docID]
	if !ok {
		return
	}
	r.mu.Lock()
	empty := len(r.clients) == 0
	r.mu.Unlock()
	if empty {
		r.close()
		delete(h.rooms, docID)
	}
}

// RoomCount reports how many rooms are currently live (used by tests and metrics).
func (h *Hub) RoomCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}
