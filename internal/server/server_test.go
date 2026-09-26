package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/1morshed1/collab-crdt/internal/crdt"
	"github.com/gorilla/websocket"
)

// These tests exercise the real HTTP + WebSocket stack end to end, with no database or
// Redis (nil Store, nil Bus). They prove the plumbing carries ops between clients and
// that two clients converge over the wire — the same guarantee the crdt package tests
// prove in-process, now across sockets.

// testClient is a thin WebSocket test harness that mirrors what the JS client does: it
// keeps a local CRDT replica, applies incoming ops, and can send local edits.
type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	doc  *crdt.Document
	id   string
}

func dial(t *testing.T, srv *httptest.Server, doc, name string) *testClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?doc=" + doc + "&name=" + name
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tc := &testClient{t: t, conn: conn}

	// First message must be init; use its client id and snapshot to seed our replica.
	msg := tc.read()
	if msg.Type != TypeInit {
		t.Fatalf("expected init, got %q", msg.Type)
	}
	var init InitData
	if err := json.Unmarshal(msg.Data, &init); err != nil {
		t.Fatalf("init decode: %v", err)
	}
	tc.id = init.ClientID
	// Crucial: seed our replica with OUR assigned client id, not the snapshot's site,
	// so the ids we generate never collide with another client (see crdt decision D5).
	d, err := crdt.LoadSnapshotAs(init.Snapshot, init.ClientID)
	if err != nil {
		t.Fatalf("load init snapshot: %v", err)
	}
	tc.doc = d
	return tc
}

func (tc *testClient) read() Message {
	tc.t.Helper()
	_ = tc.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := tc.conn.ReadMessage()
	if err != nil {
		tc.t.Fatalf("read: %v", err)
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		tc.t.Fatalf("decode: %v", err)
	}
	return m
}

// typeAtEnd inserts s at the end of the client's local doc and sends each op.
func (tc *testClient) typeAtEnd(s string) {
	tc.t.Helper()
	for _, r := range s {
		after := tc.doc.OffsetToAfterID(tc.doc.Len())
		op := tc.doc.LocalInsert(after, string(r))
		data, _ := json.Marshal(op)
		env, _ := json.Marshal(Message{Type: TypeOp, Data: data})
		if err := tc.conn.WriteMessage(websocket.TextMessage, env); err != nil {
			tc.t.Fatalf("write: %v", err)
		}
	}
}

// applyUntil reads messages, applying any ops, until the local text equals want or a
// deadline passes.
func (tc *testClient) applyUntil(want string) {
	tc.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for tc.doc.Text() != want {
		if time.Now().After(deadline) {
			tc.t.Fatalf("timeout waiting for %q, have %q", want, tc.doc.Text())
		}
		msg := tc.read()
		if msg.Type == TypeOp {
			var op crdt.Operation
			if err := json.Unmarshal(msg.Data, &op); err != nil {
				tc.t.Fatalf("op decode: %v", err)
			}
			tc.doc.Apply(op)
		}
		// ignore presence/cursor messages for this test
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	hub := NewHub(nil, nil, "test-instance")
	// Static dir doesn't matter for WS tests; point at a temp dir.
	s := New(hub, t.TempDir())
	return httptest.NewServer(s.Handler())
}

func TestWebSocketRelaysOps(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	alice := dial(t, srv, "doc1", "alice")
	bob := dial(t, srv, "doc1", "bob")
	defer alice.conn.Close()
	defer bob.conn.Close()

	// Bob should receive a presence update when he joins; drain any presence messages
	// isn't necessary — applyUntil ignores non-op messages.

	alice.typeAtEnd("hello")
	bob.applyUntil("hello")

	if bob.doc.Text() != "hello" {
		t.Fatalf("bob has %q", bob.doc.Text())
	}
}

func TestWebSocketConvergenceTwoWay(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	alice := dial(t, srv, "doc2", "alice")
	bob := dial(t, srv, "doc2", "bob")
	defer alice.conn.Close()
	defer bob.conn.Close()

	// Both type concurrently at the start of an empty doc.
	alice.typeAtEnd("AAA")
	bob.typeAtEnd("BBB")

	// Each must end up seeing all 6 characters, in the same order.
	// We can't know the exact interleaving a priori, so read until both have length 6,
	// then compare.
	waitLen6 := func(tc *testClient) {
		deadline := time.Now().Add(3 * time.Second)
		for len([]rune(tc.doc.Text())) < 6 {
			if time.Now().After(deadline) {
				tc.t.Fatalf("timeout: %q", tc.doc.Text())
			}
			msg := tc.read()
			if msg.Type == TypeOp {
				var op crdt.Operation
				_ = json.Unmarshal(msg.Data, &op)
				tc.doc.Apply(op)
			}
		}
	}
	waitLen6(alice)
	waitLen6(bob)

	if alice.doc.Text() != bob.doc.Text() {
		t.Fatalf("diverged over the wire: alice=%q bob=%q", alice.doc.Text(), bob.doc.Text())
	}
	t.Logf("converged over the wire to %q", alice.doc.Text())
}

func TestRoomRetiredWhenEmpty(t *testing.T) {
	hub := NewHub(nil, nil, "test-instance")
	s := New(hub, t.TempDir())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := dial(t, srv, "ephemeral", "solo")
	if hub.RoomCount() != 1 {
		t.Fatalf("want 1 room, got %d", hub.RoomCount())
	}
	c.conn.Close()

	// Give the server a moment to notice the disconnect and retire the room.
	deadline := time.Now().Add(3 * time.Second)
	for hub.RoomCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("room not retired, count=%d", hub.RoomCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var _ = http.StatusOK // keep net/http imported for readability parity with server.go
