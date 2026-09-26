package server

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// Timeouts for the WebSocket. These follow the standard gorilla/websocket pattern:
// the server pings periodically, and if a client doesn't answer with a pong within
// the read deadline we consider it gone. This is how we detect a browser tab that was
// closed or a laptop that went to sleep without a clean disconnect.
const (
	writeWait  = 10 * time.Second    // max time allowed to write one message
	pongWait   = 60 * time.Second    // must hear from the client within this window
	pingPeriod = (pongWait * 9) / 10 // ping a bit more often than pongWait
	maxMessage = 1 << 20             // 1 MiB, generous cap on a single frame
)

// Client is one WebSocket connection: a single browser tab editing one document. Its
// id doubles as the CRDT Site id for that tab, so ids the tab generates are globally
// unique.
//
// Concurrency model (important): a *Client is touched by two goroutines — readPump and
// writePump — but they never touch the same fields, so no lock is needed here. All
// shared document state lives in the Room, which has its own lock. Outbound messages
// go through the buffered `send` channel; the writePump is the ONLY goroutine that
// writes to the socket, which gorilla requires.
type Client struct {
	id   string
	conn *websocket.Conn
	room *Room
	send chan []byte
	info PresenceInfo
}

// enqueue tries to hand a pre-encoded message to this client's write pump. If the
// client's buffer is full (a slow or stuck consumer), we drop the connection rather
// than block the whole room — a single slow client must never stall broadcasts to
// everyone else. Returns false if the client was dropped.
func (c *Client) enqueue(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		// Buffer full: this client can't keep up. Close its send channel to signal
		// the write pump to exit; the room will clean it up.
		return false
	}
}

// readPump reads messages from the socket until it closes or errors, dispatching each
// to the room. It runs in its own goroutine, one per client.
func (c *Client) readPump() {
	defer func() {
		c.room.remove(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	// Each pong resets the read deadline: proof the client is still alive.
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("client %s read error: %v", c.id, err)
			}
			return
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("client %s sent malformed message: %v", c.id, err)
			continue
		}
		c.dispatch(msg)
	}
}

// dispatch routes one inbound message to the appropriate room handler. Unknown types
// are ignored rather than fatal, so the protocol can grow without breaking old peers.
func (c *Client) dispatch(msg Message) {
	switch msg.Type {
	case TypeOp:
		c.room.handleOp(c, msg.Data)
	case TypeCursor:
		c.room.handleCursor(c, msg.Data)
	default:
		log.Printf("client %s sent unknown message type %q", c.id, msg.Type)
	}
}

// writePump is the single goroutine that writes to the socket: outbound room
// broadcasts plus periodic pings. It exits when the send channel is closed (clean
// shutdown) or a write fails.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Room closed the channel: send a clean close frame and stop.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
