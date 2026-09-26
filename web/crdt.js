// crdt.js — a faithful JavaScript port of the RGA CRDT in internal/crdt.
//
// This is deliberately a mirror of the Go implementation (document.go), field for field
// and rule for rule, because BOTH sides must build the exact same tree from the same
// operations or they won't converge. If you change the integration rule here, change it
// in Go too (and vice-versa). Read docs/01 for the algorithm; this file assumes it.
//
// No build step, no framework — this is a plain ES module the browser loads directly.

// A CharID is { lamport: number, site: string }. We key maps by a compact string form.
export const ROOT = { lamport: 0, site: "" };

function idKey(id) {
  return id.lamport + "@" + id.site;
}
function isRoot(id) {
  return id.lamport === 0 && id.site === "";
}
// Total order over CharIDs: lamport first, then site. Must match Go's CharID.Less.
function less(a, b) {
  if (a.lamport !== b.lamport) return a.lamport < b.lamport;
  return a.site < b.site;
}
function equal(a, b) {
  return a.lamport === b.lamport && a.site === b.site;
}

export class Document {
  constructor(site) {
    this.site = site;
    this.lamport = 0;
    this.root = { id: ROOT, value: "", deleted: false, parent: ROOT, children: [] };
    this.nodes = new Map();
    this.nodes.set(idKey(ROOT), this.root);
    // Out-of-order buffers (see docs/01 §6): ops that reference a not-yet-known char.
    this.pendingChildren = new Map(); // parentKey -> [op]
    this.pendingDeletes = new Map();  // targetKey -> true
  }

  // ---- local editing: generate a fresh id, apply, return the op to broadcast ----

  localInsert(after, value) {
    this.lamport += 1;
    const op = {
      type: "insert",
      id: { lamport: this.lamport, site: this.site },
      after,
      value,
      target: ROOT,
    };
    this.apply(op);
    return op;
  }

  localDelete(target) {
    const op = { type: "delete", id: ROOT, after: ROOT, value: "", target };
    this.apply(op);
    return op;
  }

  // ---- applying ops (local or remote); idempotent and order-independent ----

  apply(op) {
    if (op.type === "insert") {
      if (op.id.lamport > this.lamport) this.lamport = op.id.lamport;
      this.#applyInsert(op);
    } else if (op.type === "delete") {
      this.#applyDelete(op);
    }
  }

  #applyInsert(op) {
    const key = idKey(op.id);
    if (this.nodes.has(key)) return; // idempotent: already have it

    const parent = this.nodes.get(idKey(op.after));
    if (!parent) {
      // Parent not here yet — park until it arrives.
      const pk = idKey(op.after);
      const arr = this.pendingChildren.get(pk) || [];
      arr.push(op);
      this.pendingChildren.set(pk, arr);
      return;
    }

    const node = { id: op.id, value: op.value, deleted: false, parent: op.after, children: [] };
    if (this.pendingDeletes.has(key)) {
      node.deleted = true;
      this.pendingDeletes.delete(key);
    }
    this.nodes.set(key, node);
    this.#insertChildSorted(parent, node);

    // Flush any children that were waiting for this node.
    const waiting = this.pendingChildren.get(key);
    if (waiting) {
      this.pendingChildren.delete(key);
      for (const w of waiting) this.#applyInsert(w);
    }
  }

  #applyDelete(op) {
    const key = idKey(op.target);
    const node = this.nodes.get(key);
    if (!node) {
      this.pendingDeletes.set(key, true); // delete before target arrived
      return;
    }
    node.deleted = true; // tombstone; never unlink
  }

  // Keep children sorted by id DESCENDING (higher id first) — matches Go's rule (D2).
  #insertChildSorted(parent, node) {
    let i = 0;
    while (i < parent.children.length && !less(parent.children[i].id, node.id)) {
      i++;
    }
    parent.children.splice(i, 0, node);
  }

  // ---- reading ----

  #walk(node, visit) {
    if (!isRoot(node.id)) visit(node);
    for (const c of node.children) this.#walk(c, visit);
  }

  text() {
    let s = "";
    this.#walk(this.root, (n) => {
      if (!n.deleted) s += n.value;
    });
    return s;
  }

  #visibleNodes() {
    const out = [];
    this.#walk(this.root, (n) => {
      if (!n.deleted) out.push(n);
    });
    return out;
  }

  len() {
    return this.#visibleNodes().length;
  }

  // ---- cursor <-> offset (see docs/02) ----

  offsetToAfterID(offset) {
    const vis = this.#visibleNodes();
    if (offset <= 0) return ROOT;
    if (offset > vis.length) offset = vis.length;
    return vis[offset - 1].id;
  }

  afterIDToOffset(after) {
    if (isRoot(after)) return 0;
    let offset = 0;
    let found = 0;
    this.#walk(this.root, (n) => {
      if (equal(n.id, after)) {
        found = n.deleted ? offset : offset + 1;
      }
      if (!n.deleted) offset++;
    });
    return found;
  }

  targetAtOffset(i) {
    const vis = this.#visibleNodes();
    if (i < 0 || i >= vis.length) return null;
    return vis[i].id;
  }

  // ---- load a server snapshot, keeping OUR site id (critical: see decision D5) ----

  loadSnapshotKeepingSite(snap) {
    // snap: { site_id, lamport, nodes: [{id, value, deleted, parent}] }
    // Replay inserts (parking handles any child-before-parent ordering), then deletes.
    for (const sn of snap.nodes || []) {
      this.#applyInsert({ type: "insert", id: sn.id, after: sn.parent, value: sn.value, target: ROOT });
    }
    for (const sn of snap.nodes || []) {
      if (sn.deleted) this.#applyDelete({ type: "delete", target: sn.id });
    }
    if ((snap.lamport || 0) > this.lamport) this.lamport = snap.lamport;
  }
}

export { idKey, isRoot, equal, less };
