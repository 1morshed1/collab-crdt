// editor.js — binds a plain <textarea> to the CRDT and renders collaborators.
//
// The hardest UI problem in a collaborative editor is turning raw textarea changes into
// CRDT operations, and applying REMOTE operations without yanking the local caret
// around. This file solves both, plus draws remote cursors and the presence roster.
//
// Strategy for local edits: instead of intercepting every keystroke (fragile — think
// paste, IME, autocomplete, drag), we DIFF the textarea's new value against its previous
// value on each `input` event. The difference is always a single contiguous
// replacement, which we translate into deletes + inserts on the CRDT. This one code path
// handles typing, pasting, deleting, cutting, and selecting-then-typing uniformly.
//
// A note on units: the CRDT counts positions in Unicode code points (like Go runes),
// but a textarea's selectionStart is in UTF-16 code units. We convert at the boundary so
// emoji and other astral characters don't corrupt offsets.

import { ROOT } from "./crdt.js";

// --- Unicode helpers: convert between UTF-16 indices and code-point offsets ---
const cp = (s) => Array.from(s); // split into code points
const utf16ToCp = (s, idx) => cp(s.slice(0, idx)).length;
const cpToUtf16 = (s, n) => cp(s).slice(0, n).join("").length;

export class Editor {
  constructor({ doc, conn, dom }) {
    this.doc = doc;
    this.conn = conn;
    this.textarea = dom.textarea;
    this.overlay = dom.overlay; // absolutely-positioned layer for remote carets
    this.mirror = dom.mirror; // hidden div used to measure caret pixel positions
    this.roster = dom.roster; // presence chips container
    this.status = dom.status; // connection status pill

    this.lastValue = "";
    this.clientID = "";
    this.remoteCursors = new Map(); // clientID -> {anchor, head}
    this.presence = new Map(); // clientID -> info

    this.textarea.addEventListener("input", () => this.onInput());
    // Any caret movement should tell peers where we are.
    for (const ev of ["keyup", "click", "select"]) {
      this.textarea.addEventListener(ev, () => this.sendCursor());
    }
    document.addEventListener("selectionchange", () => {
      if (document.activeElement === this.textarea) this.sendCursor();
    });
    window.addEventListener("resize", () => this.renderRemoteCursors());
    this.textarea.addEventListener("scroll", () => this.renderRemoteCursors());
  }

  // ---- server messages ----

  onInit(data) {
    this.clientID = data.client_id;
    // Rebuild our replica from the snapshot but keep OUR site id (decision D5).
    this.doc.site = data.client_id;
    this.doc.loadSnapshotKeepingSite(data.snapshot || { nodes: [] });
    const text = this.doc.text();
    this.textarea.value = text;
    this.lastValue = text;
    this.textarea.disabled = false;
    this.onPresence({ users: data.presence || [] });
    this.renderRemoteCursors();
  }

  onOp(op) {
    // Preserve the local caret across the remote change by anchoring it to a char id.
    const sel = this.captureSelection();
    this.doc.apply(op);
    this.restore(sel);
  }

  onCursor(c) {
    if (c.client_id && c.client_id !== this.clientID) {
      this.remoteCursors.set(c.client_id, { anchor: c.anchor, head: c.head });
      this.renderRemoteCursors();
    }
  }

  onPresence(data) {
    this.presence = new Map();
    for (const u of data.users || []) this.presence.set(u.client_id, u);
    // Drop cursors for anyone who left.
    for (const id of [...this.remoteCursors.keys()]) {
      if (!this.presence.has(id)) this.remoteCursors.delete(id);
    }
    this.renderRoster();
    this.renderRemoteCursors();
  }

  onStatus(s) {
    if (!this.status) return;
    this.status.textContent = s;
    this.status.dataset.state = s;
    if (s !== "connected") this.textarea.disabled = true;
  }

  // ---- local edits: diff old vs new and emit ops ----

  onInput() {
    const oldCP = cp(this.lastValue);
    const newCP = cp(this.textarea.value);

    // Common prefix length (in code points).
    let p = 0;
    while (p < oldCP.length && p < newCP.length && oldCP[p] === newCP[p]) p++;
    // Common suffix length, not overlapping the prefix.
    let s = 0;
    while (
      s < oldCP.length - p &&
      s < newCP.length - p &&
      oldCP[oldCP.length - 1 - s] === newCP[newCP.length - 1 - s]
    ) {
      s++;
    }

    const removedCount = oldCP.length - s - p;
    const inserted = newCP.slice(p, newCP.length - s);

    // 1) Deletions: collect the target ids FIRST (offsets shift as we delete).
    if (removedCount > 0) {
      const targets = [];
      for (let i = 0; i < removedCount; i++) {
        const id = this.doc.targetAtOffset(p + i);
        if (id) targets.push(id);
      }
      for (const t of targets) {
        const op = this.doc.localDelete(t);
        this.conn.sendOp(op);
      }
    }

    // 2) Insertions: anchor after the shared prefix, then chain each new char.
    if (inserted.length > 0) {
      let after = this.doc.offsetToAfterID(p);
      for (const ch of inserted) {
        const op = this.doc.localInsert(after, ch);
        this.conn.sendOp(op);
        after = op.id;
      }
    }

    this.lastValue = this.textarea.value;
    this.sendCursor();
  }

  // ---- caret preservation across remote edits ----

  captureSelection() {
    const v = this.textarea.value;
    const startCP = utf16ToCp(v, this.textarea.selectionStart);
    const endCP = utf16ToCp(v, this.textarea.selectionEnd);
    return {
      anchorStart: this.doc.offsetToAfterID(startCP),
      anchorEnd: this.doc.offsetToAfterID(endCP),
    };
  }

  restore(sel) {
    const text = this.doc.text();
    this.textarea.value = text;
    this.lastValue = text;
    if (sel) {
      const startCP = this.doc.afterIDToOffset(sel.anchorStart);
      const endCP = this.doc.afterIDToOffset(sel.anchorEnd);
      const start = cpToUtf16(text, startCP);
      const end = cpToUtf16(text, endCP);
      try {
        this.textarea.setSelectionRange(start, end);
      } catch {
        /* textarea may be blurred; ignore */
      }
    }
    this.renderRemoteCursors();
  }

  // ---- cursor reporting (throttled) ----

  sendCursor() {
    if (this._cursorTimer) return;
    this._cursorTimer = setTimeout(() => {
      this._cursorTimer = null;
      const v = this.textarea.value;
      const anchor = this.doc.offsetToAfterID(utf16ToCp(v, this.textarea.selectionStart));
      const head = this.doc.offsetToAfterID(utf16ToCp(v, this.textarea.selectionEnd));
      this.conn.sendCursor(anchor, head);
    }, 60);
  }

  // ---- rendering: presence roster + remote carets ----

  renderRoster() {
    if (!this.roster) return;
    this.roster.innerHTML = "";
    for (const [id, info] of this.presence) {
      const chip = document.createElement("span");
      chip.className = "chip";
      chip.style.setProperty("--chip", info.color || "#888");
      const isMe = id === this.clientID;
      chip.textContent = (info.name || "Anon") + (isMe ? " (you)" : "");
      this.roster.appendChild(chip);
    }
  }

  // Compute the pixel position of a UTF-16 index within the textarea using a hidden
  // mirror element styled identically. This is the classic "caret coordinates" trick.
  caretPixels(index) {
    const ta = this.textarea;
    const style = getComputedStyle(ta);
    const m = this.mirror;
    // Copy the properties that affect text layout.
    for (const prop of [
      "boxSizing", "width", "paddingTop", "paddingRight", "paddingBottom", "paddingLeft",
      "borderTopWidth", "borderRightWidth", "borderBottomWidth", "borderLeftWidth",
      "fontFamily", "fontSize", "fontWeight", "fontStyle", "letterSpacing", "lineHeight",
      "textTransform", "wordSpacing", "tabSize",
    ]) {
      m.style[prop] = style[prop];
    }
    m.style.position = "absolute";
    m.style.visibility = "hidden";
    m.style.whiteSpace = "pre-wrap";
    m.style.overflowWrap = "break-word";
    m.style.top = "0";
    m.style.left = "0";

    const value = ta.value;
    m.textContent = value.slice(0, index);
    const marker = document.createElement("span");
    marker.textContent = "​"; // zero-width space
    m.appendChild(marker);

    const top = marker.offsetTop - ta.scrollTop;
    const left = marker.offsetLeft - ta.scrollLeft;
    m.removeChild(marker);
    return { top, left, lineHeight: parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.2 };
  }

  renderRemoteCursors() {
    if (!this.overlay) return;
    this.overlay.innerHTML = "";
    const text = this.doc.text();
    for (const [id, cur] of this.remoteCursors) {
      const info = this.presence.get(id);
      if (!info) continue;
      const color = info.color || "#e6194B";
      const anchorCP = this.doc.afterIDToOffset(cur.anchor);
      const headCP = this.doc.afterIDToOffset(cur.head);

      // Draw a selection highlight if anchor != head (single-line approximation: we draw
      // a caret at the head; a full multi-line selection rectangle is left as an
      // exercise — see docs/06).
      const idx = cpToUtf16(text, headCP);
      const { top, left, lineHeight } = this.caretPixels(idx);

      const caret = document.createElement("div");
      caret.className = "remote-caret";
      caret.style.background = color;
      caret.style.top = top + "px";
      caret.style.left = left + "px";
      caret.style.height = lineHeight + "px";

      const label = document.createElement("span");
      label.className = "remote-label";
      label.style.background = color;
      label.textContent = info.name || "Anon";
      caret.appendChild(label);

      this.overlay.appendChild(caret);
      void anchorCP; // reserved for future selection-range rendering
    }
  }
}

export { ROOT };
