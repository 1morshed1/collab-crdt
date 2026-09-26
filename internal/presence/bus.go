// Package presence provides the Redis-backed cross-instance message bus that lets
// multiple server pods collaborate on the same document. It implements the server.Bus
// interface (declared in internal/server). See docs/05.
package presence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Why this exists: a single Go process broadcasts an op to its own clients just fine
// (docs/03). But if you run several server pods behind a load balancer, two people
// editing the same document may connect to different pods. Pod A's in-memory broadcast
// never reaches pod B's clients. Redis Pub/Sub bridges them: every pod publishes a
// document's messages to a channel, and every pod subscribes to the documents it has
// clients for. The CRDT's order-independence (docs/01) is what makes this safe — pods
// can receive ops in different orders and still converge.

// channelPrefix namespaces our pub/sub channels so they don't clash with anything else
// sharing the Redis instance. A document's channel is e.g. "collab:doc:notes".
const channelPrefix = "collab:doc:"

// envelope wraps a payload with the id of the instance that published it, so a pod can
// recognize and drop its OWN messages (Redis Pub/Sub delivers a publish to every
// subscriber, including the publisher). Without this, a pod would re-broadcast its own
// ops to its clients twice, and two pods could echo forever.
type envelope struct {
	Origin  string          `json:"origin"`
	Payload json.RawMessage `json:"payload"`
}

// RedisBus implements server.Bus using Redis Pub/Sub.
type RedisBus struct {
	rdb        *redis.Client
	instanceID string
}

// NewRedisBus connects to Redis at addr (e.g. "localhost:6379") and returns a bus
// tagged with instanceID (this pod's unique id, used for echo suppression).
func NewRedisBus(ctx context.Context, addr, instanceID string) (*RedisBus, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisBus{rdb: rdb, instanceID: instanceID}, nil
}

// Close releases the Redis client.
func (b *RedisBus) Close() error { return b.rdb.Close() }

// Publish wraps the payload with this instance's id and publishes it to the document's
// channel. Every subscribed pod (including this one) receives it; this pod's own
// Subscribe loop drops it via the origin check.
func (b *RedisBus) Publish(ctx context.Context, docID string, data []byte) error {
	env, err := json.Marshal(envelope{Origin: b.instanceID, Payload: data})
	if err != nil {
		return err
	}
	return b.rdb.Publish(ctx, channelPrefix+docID, env).Err()
}

// Subscribe returns a channel of payloads published to docID by OTHER instances, plus a
// cancel func. Messages this instance published are filtered out, so whatever comes out
// of the returned channel is always "from someone else" and can be broadcast to local
// clients as-is (matching what server.Bus promises).
//
// The subscription runs a goroutine that reads Redis messages, unwraps the envelope,
// drops echoes, and forwards the rest. Calling cancel closes the Redis subscription;
// the goroutine then exits and closes the output channel.
func (b *RedisBus) Subscribe(ctx context.Context, docID string) (<-chan []byte, func(), error) {
	sub := b.rdb.Subscribe(ctx, channelPrefix+docID)
	// Wait for the subscription to be established so we don't miss early messages.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, nil, fmt.Errorf("redis subscribe: %w", err)
	}

	out := make(chan []byte, 64)
	done := make(chan struct{})

	go func() {
		defer close(out)
		ch := sub.Channel()
		for {
			select {
			case <-done:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var env envelope
				if json.Unmarshal([]byte(msg.Payload), &env) != nil {
					continue
				}
				if env.Origin == b.instanceID {
					continue // our own echo; ignore
				}
				select {
				case out <- env.Payload:
				case <-done:
					return
				}
			}
		}
	}()

	cancel := func() {
		close(done)
		_ = sub.Close()
	}
	return out, cancel, nil
}
