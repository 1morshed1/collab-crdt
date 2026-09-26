# 06 — The browser client: textarea → CRDT → screen

> Code: `web/crdt.js`, `web/net.js`, `web/editor.js`, `web/main.js`,
> `web/index.html`, `web/styles.css`. No build step — the browser loads these ES
> modules directly.

The client is where the abstract CRDT becomes something you can type into. It has four
parts, each a small module:

| File | Job |
|------|-----|
| `crdt.js` | A faithful **JS port of the Go CRDT** — same tree, same rules |
| `net.js` | The **WebSocket** connection, with auto-reconnect |
| `editor.js` | Binds a `<textarea>` to the CRDT; draws remote cursors & presence |
| `main.js` | Wires the three together and reads the document id from the URL |

## Why a second copy of the CRDT (in JavaScript)?

Each client keeps its **own full replica** so edits apply **locally and instantly** — you
never wait for a server round-trip to see your own keystroke. That's the latency win CRDTs
buy us. For your replica and everyone else's to agree, the JS integration rule must be
**byte-for-byte identical** to the Go one: same `(lamport, site)` ordering, same
"children sorted descending," same tombstone handling, same out-of-order parking.
`crdt.js` is intentionally a line-by-line mirror of `document.go`. If you change one, change
the other — a mismatch would silently break convergence. (This is a real cost of the
"replica everywhere" model; production libraries like Yjs ship one implementation compiled
to both targets to avoid drift.)

The one deliberate difference is `loadSnapshotKeepingSite`: the client loads the shared
document but keeps the **server-assigned client id** as its site, never the snapshot's
site — decision **D5**, the id-collision bug from doc 01/03.

## The hard part: turning textarea changes into operations

You could try to intercept every keystroke, but that path is a swamp: paste, cut,
drag-drop, autocomplete, IME composition, select-all-then-type. Instead we use one robust
trick — **diffing** (`editor.js`, `onInput`):

1. Remember the textarea's previous value.
2. On every `input` event, compare it to the new value.
3. Any edit — however it happened — shows up as a single contiguous replacement: a shared
   **prefix**, a shared **suffix**, and a differing middle. Compute those two lengths.
4. The removed middle → **delete** ops (collect the target CharIDs first, since offsets
   shift as you delete). The inserted middle → **insert** ops, chained after the prefix.
5. Send each op over the WebSocket. Your local replica was already updated by
   `localInsert`/`localDelete`, and the textarea already shows the new text, so there's
   nothing to redraw for your own edit.

This single code path handles typing, pasting, deleting, and replacing selections
uniformly. It's ~20 lines and far more reliable than key handling.

### Unicode: code points vs UTF-16

The CRDT counts positions in **code points** (like Go runes), but a textarea's
`selectionStart` is in **UTF-16 code units**. For ASCII they're identical; for emoji they
diverge. `editor.js` converts at the boundary (`utf16ToCp` / `cpToUtf16`) so astral
characters don't corrupt offsets. A small detail that saves a category of confusing bugs.

## The other hard part: applying remote edits without moving your caret

When a peer's op arrives, we must update the text **without** yanking your cursor around.
The fix uses the same idea as everything else — anchor to an **id**, not an offset
(`captureSelection` / `restore`):

1. Before applying the remote op, record the CharID your caret sits after
   (`offsetToAfterID`).
2. Apply the op; recompute the text.
3. Set the textarea value, then translate the anchor id **back** to an offset
   (`afterIDToOffset`) and restore the selection there.

Because the anchor is stable across inserts/deletes elsewhere, your caret stays put
relative to the character it was next to — exactly what you'd expect when a collaborator
edits above you.

## Live remote cursors (the "multiplayer" feel)

Peers send `cursor` messages carrying their caret/selection as CharIDs. To draw them we
need pixel coordinates for a text offset in a textarea — which the DOM doesn't expose
directly. `caretPixels` uses the classic **mirror-div** technique: a hidden div styled
identically to the textarea, filled with the text up to the caret plus a marker span; the
marker's `offsetTop`/`offsetLeft` (minus scroll) give the caret's pixel position. We paint
a colored bar + name label in a non-interactive overlay layer that sits exactly over the
textarea.

This is verified working in the headless smoke test (two tabs, remote caret rendered).
Two honest limitations, left as documented exercises:

- Only the **caret** (selection head) is drawn, not a full highlighted selection
  rectangle across multiple lines.
- The mirror must track the textarea's exact geometry; extreme font/zoom changes can
  nudge alignment.

## Resilience: reconnect and resync for free

`net.js` reconnects automatically with exponential backoff. On reconnect the server sends
a fresh `init` snapshot and the client rebuilds its replica. Combined with CRDT
idempotency and the durable op log (docs/04), a dropped connection causes **no lost or
duplicated edits** — you can go offline, keep typing, and converge on reconnect. That
robustness falling out naturally is one of the nicest payoffs of the whole design.

## Try it / how it was verified

Open two tabs at `http://localhost:8080/?doc=notes` and type in both. The repo's smoke
tests (`/tmp` scripts during development, and the Go WS tests) confirmed:

- Alice types → Bob sees it; Bob appends → Alice sees it.
- Both type at the **same spot concurrently** → both converge to identical text.
- Presence roster shows both people; a remote caret renders.
- Text **persists**: close all tabs (room retires), reopen → the server reloads from
  Postgres and the text is back.

Next: `docs/07-running-and-ops.md` — Docker Compose, configuration, and running it
yourself.
