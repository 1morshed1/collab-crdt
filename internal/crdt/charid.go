// Package crdt implements an RGA (Replicated Growable Array) sequence CRDT for
// collaborative text editing.
//
// The whole package is dependency-free on purpose: the CRDT is the intellectual
// core of the project, and keeping it isolated (no WebSocket, no database, no
// framework) makes it easy to test exhaustively and easy to read. Everything else
// in the repo is plumbing around this package.
//
// Read docs/01-crdt-rga.md alongside this code.
package crdt

import "fmt"

// CharID is the globally-unique, permanent identity of a single character in the
// document. This is THE central idea of the whole system: we never identify a
// position by its integer offset (offsets shift as other people edit); instead
// every character carries an identity that never changes for its entire life.
//
// A CharID is a Lamport timestamp paired with the id of the replica (site) that
// created it. The pair is unique because:
//   - a single site never issues the same Lamport value twice (it increments on
//     every insert), and
//   - two different sites are distinguished by SiteID even if they happen to pick
//     the same Lamport value concurrently.
type CharID struct {
	// Lamport is a logical clock value (see docs/01). It gives us a notion of
	// "happened-before" without any wall clocks, which would be useless across
	// machines whose clocks disagree.
	Lamport uint64 `json:"lamport"`
	// Site is the unique identifier of the replica that created this character.
	Site string `json:"site"`
}

// Root is the sentinel identity for the (invisible) document root. Every real
// character is ultimately a descendant of Root. "Insert at the very beginning of
// the document" means "insert as a child of Root". Root itself holds no character.
var Root = CharID{Lamport: 0, Site: ""}

// IsRoot reports whether this id is the document root sentinel.
func (id CharID) IsRoot() bool {
	return id.Lamport == 0 && id.Site == ""
}

// Less defines a TOTAL order over CharIDs: compare Lamport first, then break ties
// with Site. Two things matter about this ordering:
//
//  1. It is total and deterministic — every replica computes the same answer for
//     any two ids. That is what lets concurrent inserts at the same spot resolve
//     to the same visual order everywhere, with no coordination.
//  2. Lamport-first means a "newer" edit (higher logical time) sorts higher. We
//     use that in the sibling ordering rule (see document.go): among characters
//     inserted after the same parent, the higher CharID is placed first.
//
// The exact direction is a convention, not a correctness requirement — any total
// order that everyone agrees on converges. We just have to be consistent. See
// decision D2 in docs/decisions.md.
func (id CharID) Less(other CharID) bool {
	if id.Lamport != other.Lamport {
		return id.Lamport < other.Lamport
	}
	return id.Site < other.Site
}

// Equal reports whether two ids are the same character identity.
func (id CharID) Equal(other CharID) bool {
	return id.Lamport == other.Lamport && id.Site == other.Site
}

// String renders a compact, human-readable form used in logs and test output,
// e.g. "3@bob" or "ROOT".
func (id CharID) String() string {
	if id.IsRoot() {
		return "ROOT"
	}
	return fmt.Sprintf("%d@%s", id.Lamport, id.Site)
}
