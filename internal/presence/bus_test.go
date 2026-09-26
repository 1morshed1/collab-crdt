package presence

import (
	"context"
	"os"
	"testing"
	"time"
)

// Integration tests: set TEST_REDIS_ADDR (e.g. localhost:6380) to run. They skip
// otherwise so `go test ./...` stays green with no Redis.

func testBus(t *testing.T, instanceID string) *RedisBus {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run presence bus tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := NewRedisBus(ctx, addr, instanceID)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestCrossInstanceDelivery: a message published by instance A is delivered to a
// subscriber on instance B.
func TestCrossInstanceDelivery(t *testing.T) {
	a := testBus(t, "A")
	b := testBus(t, "B")
	ctx := context.Background()

	ch, cancel, err := b.Subscribe(ctx, "doc-x")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	want := []byte(`{"type":"op","data":{}}`)
	if err := a.Publish(ctx, "doc-x", want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-ch:
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cross-instance message")
	}
}

// TestEchoSuppression: an instance does NOT receive its own publishes back.
func TestEchoSuppression(t *testing.T) {
	a := testBus(t, "A")
	ctx := context.Background()

	ch, cancel, err := a.Subscribe(ctx, "doc-y")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	if err := a.Publish(ctx, "doc-y", []byte(`{"v":"mine"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-ch:
		t.Fatalf("received own echo: %q", got)
	case <-time.After(500 * time.Millisecond):
		// good: no echo delivered
	}
}

// TestChannelIsolation: a subscriber to one document does not receive another
// document's messages.
func TestChannelIsolation(t *testing.T) {
	a := testBus(t, "A")
	b := testBus(t, "B")
	ctx := context.Background()

	ch, cancel, err := b.Subscribe(ctx, "doc-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	if err := a.Publish(ctx, "doc-2", []byte(`{"v":"other"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-ch:
		t.Fatalf("leaked across documents: %q", got)
	case <-time.After(500 * time.Millisecond):
		// good
	}
}
