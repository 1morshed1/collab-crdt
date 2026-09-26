package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// Server is the HTTP surface: it serves the static editor UI and upgrades WebSocket
// connections into the collaboration hub. It is intentionally thin — all the
// interesting logic lives in Hub/Room/Client. This file is just the front door.
type Server struct {
	hub      *Hub
	upgrader websocket.Upgrader
	static   http.Handler
}

// New builds a Server. staticDir is the folder holding the web client (index.html and
// friends); it is served at "/".
func New(hub *Hub, staticDir string) *Server {
	return &Server{
		hub: hub,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Same-origin only in a real deployment. For local development across two
			// browser tabs on localhost this is fine; tighten it before exposing the
			// server publicly.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		static: http.FileServer(http.Dir(staticDir)),
	}
}

// Handler returns the http.Handler mux for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", s.static)
	return mux
}

// handleWS upgrades a request to a WebSocket and attaches it to the right room. The
// document id and display name come from query parameters:
//
//	/ws?doc=<document-id>&name=<display-name>
//
// The server assigns the client id (used as the CRDT site id) and a color so those
// can't collide or be spoofed.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	docID := strings.TrimSpace(r.URL.Query().Get("doc"))
	if docID == "" {
		docID = "default"
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Anonymous"
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return
	}

	clientID := newClientID()
	client := &Client{
		id:   clientID,
		conn: conn,
		room: s.hub.room(docID),
		send: make(chan []byte, 256), // buffer absorbs bursts; overflow => client dropped
		info: PresenceInfo{
			ClientID: clientID,
			Name:     name,
			Color:    colorFor(clientID),
		},
	}

	// Start the write pump before adding to the room, so the init message the room
	// sends on add() has somewhere to go.
	go client.writePump()
	client.room.add(client)
	go client.readPump()
}

// newClientID returns a short random hex id. It is unique enough to serve as a CRDT
// site id for the life of a connection; uniqueness comes from randomness, not
// coordination.
func newClientID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "c-" + hex.EncodeToString(b)
}

// palette is a set of visually distinct, readable cursor colors.
var palette = []string{
	"#e6194B", "#3cb44b", "#4363d8", "#f58231", "#911eb4",
	"#42d4f4", "#f032e6", "#bfef45", "#fabed4", "#469990",
	"#9A6324", "#800000", "#808000", "#000075", "#a9a9a9",
}

// colorFor deterministically assigns a palette color from a client id, so a given
// connection always shows the same color to everyone.
func colorFor(id string) string {
	var sum int
	for _, b := range []byte(id) {
		sum += int(b)
	}
	return palette[sum%len(palette)]
}
