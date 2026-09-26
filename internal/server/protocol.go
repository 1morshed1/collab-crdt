package server

import (
	"encoding/json"

	"github.com/1morshed1/collab-crdt/internal/crdt"
)

// This file defines the WebSocket wire protocol: the messages a client and server
// send each other. Everything is a small JSON envelope with a "type" and a "data"
// blob, mirroring plan.md's WSMessage. Keeping the envelope uniform means the read
// loop can dispatch on a single field, and new message types can be added without
// touching the framing.

// Message is the envelope for every WebSocket frame in both directions.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Message type constants. Client->server and server->client share the envelope but
// use these to disambiguate intent.
const (
	// TypeInit is server->client, sent once right after connect. It carries the
	// starting document state and the current presence list so a late joiner is
	// immediately consistent with everyone else.
	TypeInit = "init"

	// TypeOp is bidirectional. Client->server: "I made this edit." Server->client:
	// "a peer made this edit, apply it." The payload is a crdt.Operation.
	TypeOp = "op"

	// TypeCursor is bidirectional and carries a CursorData: where a user's caret /
	// selection is, expressed as stable CharIDs (see docs/02).
	TypeCursor = "cursor"

	// TypePresence is server->client: the full list of who is currently in the room
	// (and their colors/names). Sent whenever someone joins or leaves.
	TypePresence = "presence"
)

// InitData is the payload of a TypeInit message. Snapshot is the serialized CRDT
// state (see crdt.Document.Snapshot) — the client loads it to build its local
// replica. ClientID is the unique site id the server assigned this client; the
// client uses it as its CRDT Site so that ids it generates never collide with anyone
// else's (the server guarantees uniqueness).
type InitData struct {
	ClientID string          `json:"client_id"`
	Snapshot json.RawMessage `json:"snapshot"`
	Presence []PresenceInfo  `json:"presence"`
}

// CursorData reports a user's caret and selection. Anchor is the fixed end of a
// selection (or the caret position for a plain cursor); Head is the moving end. Both
// are CharIDs so they stay meaningful as the document changes around them.
type CursorData struct {
	ClientID string      `json:"client_id"`
	Anchor   crdt.CharID `json:"anchor"`
	Head     crdt.CharID `json:"head"`
}

// PresenceInfo is one participant's public presence. It is ephemeral state — it
// lives only while the user is connected and is never persisted to Postgres.
type PresenceInfo struct {
	ClientID string      `json:"client_id"`
	Name     string      `json:"name"`
	Color    string      `json:"color"`
	Anchor   crdt.CharID `json:"anchor"`
	Head     crdt.CharID `json:"head"`
}

// PresenceData is the payload of a TypePresence message: the whole room roster.
type PresenceData struct {
	Users []PresenceInfo `json:"users"`
}

// encode is a tiny helper to build an envelope with a JSON-encoded payload.
func encode(msgType string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Message{Type: msgType, Data: data})
}
