# 02 — Operations on the wire, and cursors that don't drift

> Code: `internal/crdt/operation.go` (op format), the cursor helpers in
> `document.go` (`OffsetToAfterID`, `AfterIDToOffset`, `TargetAtOffset`).

Doc 01 built the CRDT. This short doc covers two practical bridges between the CRDT
and the outside world: **how an operation is serialized** so the Go server and the
JavaScript client speak the same language, and **how a cursor is represented** so it
stays put while other people edit around it.

## 1. One operation struct, shared by everyone

An `Operation` is the atomic unit that travels over the WebSocket and gets written to
the database. The design rule (stated in the code comments too):

> An operation must carry everything needed to apply it on **any** replica, in **any**
> order, with **no** extra context.

That rule is why the struct has no integer offsets and no "sequence number relative to
the sender" — those would only make sense on the machine that produced them. Instead:

```go
type Operation struct {
    Type   OpType // "insert" | "delete"
    ID     CharID // insert: the new char's identity
    After  CharID // insert: the parent (Root = start of document)
    Value  string // insert: the character (one code point)
    Target CharID // delete: which char to tombstone
}
```

It serializes to plain JSON, e.g.:

```json
{"type":"insert","id":{"lamport":5,"site":"tab-a1b2"},"after":{"lamport":4,"site":"tab-a1b2"},"value":"h","target":{"lamport":0,"site":""}}
{"type":"delete","id":{"lamport":0,"site":""},"after":{"lamport":0,"site":""},"target":{"lamport":5,"site":"tab-a1b2"}}
```

The JavaScript client (see `docs/06`) builds the *identical* JSON shape. Because the
CRDT logic is mirrored on both sides, a client can apply its own edit locally for
instant feedback and send the same op to the server, confident everyone will converge.

There is deliberately **no "update" op**. Changing a character = delete + insert.
Fewer op types = fewer interactions to reason about = simpler convergence.

## 2. Why cursors cannot be integers either

The exact same problem that sank integer *edit positions* (doc 01 §1) also sinks
integer *cursor positions*. Suppose your cursor is "at offset 5." Someone else inserts
three characters near the start of the document. Your cursor should still be sitting
in front of the same character it was before — but offset 5 now points three
characters too early. For your own cursor the editor fixes this for you; but for
**remote** cursors (showing where your collaborators are), and for anchoring a
selection across edits, we need something stable.

Same fix as before: **represent a cursor as a CharID** — "the cursor is immediately
after the character with id X." That anchor survives arbitrary concurrent editing.
The original spec calls these `AnchorID` (and `HeadID` for the far end of a selection).

## 3. The conversion helpers

The editor UI unavoidably thinks in offsets (a `<textarea>` reports
`selectionStart = 7`). So we convert at the boundary:

| Helper | Direction | Used for |
|--------|-----------|----------|
| `OffsetToAfterID(offset)` | offset → anchor id | user typed at offset N; find the id to insert after |
| `AfterIDToOffset(after)` | anchor id → offset | drawing a remote collaborator's caret at the right pixel |
| `TargetAtOffset(i)` | offset → char id | user pressed Backspace/Delete at offset i; find the id to tombstone |

Key details worth understanding (all in `document.go`):

- **`OffsetToAfterID(0)` returns `Root`.** Offset 0 is the start of the document, and
  "insert at the start" means "insert as a child of Root." Offsets past the end are
  clamped to the end.

- **`AfterIDToOffset` is robust to a deleted anchor.** If the character your cursor was
  anchored to has since been tombstoned by someone else, we resolve to the offset of
  the nearest still-visible character, so a remote caret never jumps somewhere absurd.

- These are **O(n)** walks of the tree today. Doc 08 discusses when that starts to
  matter and how real editors keep it O(log n).

## 4. The round-trip in practice

A single keystroke, end to end:

1. User types `h` at offset 7. The client calls `OffsetToAfterID(7)` → some anchor id.
2. Client does `LocalInsert(anchor, "h")` → applies instantly on screen, gets back an
   `Operation` with a fresh id.
3. Client sends that JSON op over the WebSocket.
4. Server applies it to the room's replica, persists it, re-broadcasts it.
5. Every other client `Apply`s it. Their `h` lands at the correct spot because it is
   anchored by id, not by "offset 7" (which might mean something else on their screen).
6. Each client redraws remote carets using `AfterIDToOffset` on the cursor positions
   its peers reported.

`TestCursorAnchorSurvivesConcurrentEdits` demonstrates step 5's stability directly:
it anchors a cursor, inserts text *before* it, and asserts the anchor still resolves
to the right place.

Next: `docs/03-websocket-server.md` — the rooms and broadcast machinery that carries
these ops between clients.
