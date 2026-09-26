package server

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/1morshed1/collab-crdt/internal/presence"
)

// This test proves the whole point of the Redis bus: two SEPARATE server processes
// (simulated here as two Hubs with two HTTP servers, each with its own instance id but
// the same Redis) stay in sync. A client on pod A types; a client connected to pod B
// sees it. Without the bus, pod A's broadcast would never leave pod A.
//
// Needs Redis: set TEST_REDIS_ADDR (e.g. localhost:6380). Skips otherwise.
func TestTwoPodsConvergeViaRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the multi-pod test")
	}
	ctx := context.Background()

	busA, err := presence.NewRedisBus(ctx, addr, "pod-A")
	if err != nil {
		t.Fatalf("bus A: %v", err)
	}
	defer busA.Close()
	busB, err := presence.NewRedisBus(ctx, addr, "pod-B")
	if err != nil {
		t.Fatalf("bus B: %v", err)
	}
	defer busB.Close()

	// Two independent pods, each with no database (nil Store) but sharing Redis.
	podA := httptest.NewServer(New(NewHub(nil, busA, "pod-A"), t.TempDir()).Handler())
	defer podA.Close()
	podB := httptest.NewServer(New(NewHub(nil, busB, "pod-B"), t.TempDir()).Handler())
	defer podB.Close()

	// Use a unique doc id so repeated runs against the same Redis don't interfere.
	doc := "shared-" + t.Name()

	alice := dial(t, podA, doc, "alice") // connected to pod A
	bob := dial(t, podB, doc, "bob")     // connected to pod B
	defer alice.conn.Close()
	defer bob.conn.Close()

	// Give both rooms a moment to establish their Redis subscriptions.
	time.Sleep(200 * time.Millisecond)

	alice.typeAtEnd("cross-pod!")
	bob.applyUntil("cross-pod!")

	if bob.doc.Text() != "cross-pod!" {
		t.Fatalf("bob (pod B) has %q, expected the text alice typed on pod A", bob.doc.Text())
	}
	t.Logf("pod A -> Redis -> pod B delivered: %q", bob.doc.Text())
}
