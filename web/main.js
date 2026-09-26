// main.js — bootstraps the editor: reads the document id, wires the CRDT, the editor
// UI, and the WebSocket connection together.

import { Document } from "./crdt.js";
import { Connection } from "./net.js";
import { Editor } from "./editor.js";

// Which document are we editing? Take it from ?doc= in the URL; default to "welcome".
// Sharing a URL with the same ?doc= means editing the same document together.
const params = new URLSearchParams(location.search);
const docID = params.get("doc") || "welcome";

// A display name: remembered in localStorage so it sticks across reloads. localStorage is
// per-viewer convenience only; nothing here depends on it being present.
let name = null;
try {
  name = localStorage.getItem("collab-name");
} catch {}
if (!name) {
  name = prompt("Your name?") || "Anonymous";
  try {
    localStorage.setItem("collab-name", name);
  } catch {}
}

// Reflect the document id in the UI and keep the URL shareable.
document.getElementById("doc-id").textContent = docID;
if (!params.get("doc")) {
  const u = new URL(location.href);
  u.searchParams.set("doc", docID);
  history.replaceState(null, "", u.toString());
}

const doc = new Document("pending"); // real site id arrives in `init`

const dom = {
  textarea: document.getElementById("editor"),
  overlay: document.getElementById("overlay"),
  mirror: document.getElementById("mirror"),
  roster: document.getElementById("roster"),
  status: document.getElementById("status"),
};

// The editor handles UI + CRDT; the connection feeds it server messages. We create the
// editor first, then the connection whose handlers delegate to it.
let editor;
const conn = new Connection({
  docID,
  name,
  handlers: {
    onInit: (d) => editor.onInit(d),
    onOp: (op) => editor.onOp(op),
    onCursor: (c) => editor.onCursor(c),
    onPresence: (p) => editor.onPresence(p),
    onStatus: (s) => editor.onStatus(s),
  },
});

editor = new Editor({ doc, conn, dom });
dom.textarea.disabled = true; // enabled once `init` arrives

// Now that the editor exists (the handlers reference it), open the connection.
conn.start();
