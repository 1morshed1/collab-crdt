package crdt

// OpType distinguishes the two kinds of edit an RGA supports. Note there is no
// "update character" op: to change a character you delete the old one and insert a
// new one. Keeping the op set tiny is part of what makes convergence tractable.
type OpType string

const (
	// OpInsert adds a new character to the document.
	OpInsert OpType = "insert"
	// OpDelete tombstones an existing character (marks it deleted without removing
	// it from the structure — see docs/01 on tombstones).
	OpDelete OpType = "delete"
)

// Operation is a single, self-contained, replicated edit. This is the unit that
// travels over the WebSocket and gets written to the operations log. It is
// deliberately serializable to plain JSON so the exact same struct is used by the
// Go server and (mirrored) by the JavaScript client.
//
// The design rule for an Operation: it must carry everything needed to apply it on
// ANY replica, in ANY order, with NO additional context. That is why positions are
// CharIDs (stable identities) rather than integer offsets (which only make sense on
// the replica that produced them at the instant they were produced).
type Operation struct {
	Type OpType `json:"type"`

	// --- Insert fields ---

	// ID is the identity of the newly-inserted character. Unused for deletes.
	ID CharID `json:"id"`
	// After is the parent: the character this new one was inserted immediately
	// after. Root means "at the beginning of the document". Unused for deletes.
	After CharID `json:"after"`
	// Value is the character itself, as a string holding a single Unicode code
	// point. (Using a string rather than a Go rune keeps JSON readable and makes
	// the client side trivial.) Unused for deletes.
	Value string `json:"value,omitempty"`

	// --- Delete fields ---

	// Target is the identity of the character to tombstone. Unused for inserts.
	Target CharID `json:"target"`
}

// NewInsert is a small constructor for an insert operation.
func NewInsert(id, after CharID, value string) Operation {
	return Operation{Type: OpInsert, ID: id, After: after, Value: value}
}

// NewDelete is a small constructor for a delete operation.
func NewDelete(target CharID) Operation {
	return Operation{Type: OpDelete, Target: target}
}
