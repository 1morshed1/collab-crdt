// net.js — the WebSocket connection to the collaboration server.
//
// Responsibilities: connect, decode the JSON envelope, hand messages to callbacks, send
// ops/cursors, and — importantly — RECONNECT automatically with backoff when the socket
// drops. On reconnect the server sends a fresh `init` snapshot, so the client rebuilds
// its replica from scratch; combined with the CRDT's idempotency this means a dropped
// connection never causes lost or duplicated edits. That resilience is a big part of why
// a CRDT is nice here.

export class Connection {
  constructor({ docID, name, handlers }) {
    this.docID = docID;
    this.name = name;
    this.handlers = handlers; // { onInit, onOp, onCursor, onPresence, onStatus }
    this.ws = null;
    this.backoff = 500; // ms, doubles up to a cap
    this.maxBackoff = 8000;
    this.closedByUser = false;
    // NOTE: we do NOT connect here. The caller wires up the editor (which the handlers
    // reference) and then calls start(), so the first onStatus callback can't fire before
    // the editor exists.
  }

  start() {
    this.connect();
  }

  url() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const params = new URLSearchParams({ doc: this.docID, name: this.name });
    return `${proto}://${location.host}/ws?${params.toString()}`;
  }

  connect() {
    this.handlers.onStatus?.("connecting");
    const ws = new WebSocket(this.url());
    this.ws = ws;

    ws.onopen = () => {
      this.backoff = 500; // reset backoff on a successful connection
      this.handlers.onStatus?.("connected");
    };

    ws.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      switch (msg.type) {
        case "init":
          this.handlers.onInit?.(msg.data);
          break;
        case "op":
          this.handlers.onOp?.(msg.data);
          break;
        case "cursor":
          this.handlers.onCursor?.(msg.data);
          break;
        case "presence":
          this.handlers.onPresence?.(msg.data);
          break;
      }
    };

    ws.onclose = () => {
      this.handlers.onStatus?.("disconnected");
      if (this.closedByUser) return;
      // Reconnect with exponential backoff. When we succeed, the server's fresh `init`
      // resynchronizes us.
      setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, this.maxBackoff);
    };

    ws.onerror = () => {
      // onclose will follow and handle reconnection.
      ws.close();
    };
  }

  #send(type, data) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    this.ws.send(JSON.stringify({ type, data }));
    return true;
  }

  sendOp(op) {
    return this.#send("op", op);
  }

  sendCursor(anchor, head) {
    return this.#send("cursor", { client_id: "", anchor, head });
  }

  close() {
    this.closedByUser = true;
    this.ws?.close();
  }
}
