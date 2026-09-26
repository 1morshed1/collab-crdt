// Command server is the entry point: it reads configuration from the environment, wires
// together the CRDT collaboration hub with optional PostgreSQL persistence and optional
// Redis fan-out, and serves the web client. See docs/07 for running it.
//
// Everything is optional except the HTTP listener:
//   - No DATABASE_URL  -> documents live in memory only (great for a quick demo).
//   - No REDIS_ADDR    -> single-instance (no cross-pod fan-out).
//
// This graceful degradation is deliberate: you can `go run ./cmd/server` with nothing
// installed and still open two tabs and collaborate.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/1morshed1/collab-crdt/internal/presence"
	"github.com/1morshed1/collab-crdt/internal/server"
	"github.com/1morshed1/collab-crdt/internal/store"
)

func main() {
	addr := env("LISTEN_ADDR", ":8080")
	staticDir := env("STATIC_DIR", "web")
	dbURL := os.Getenv("DATABASE_URL")
	redisAddr := os.Getenv("REDIS_ADDR")
	instanceID := env("INSTANCE_ID", randomID())

	ctx := context.Background()

	// Optional persistence.
	var st server.Store
	if dbURL != "" {
		s, err := store.Open(ctx, dbURL)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
		defer s.Close()
		st = s
		log.Printf("persistence: PostgreSQL enabled")
	} else {
		log.Printf("persistence: DISABLED (set DATABASE_URL to enable) — documents are in-memory only")
	}

	// Optional cross-instance bus.
	var bus server.Bus
	if redisAddr != "" {
		b, err := presence.NewRedisBus(ctx, redisAddr, instanceID)
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
		defer b.Close()
		bus = b
		log.Printf("fan-out: Redis enabled (instance %s)", instanceID)
	} else {
		log.Printf("fan-out: DISABLED (set REDIS_ADDR to enable) — single instance")
	}

	hub := server.NewHub(st, bus, instanceID)
	srv := server.New(hub, staticDir)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("shutting down…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("collab-crdt listening on %s (open http://localhost%s)", addr, normalizeAddr(addr))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "inst-" + hex.EncodeToString(b)
}

// normalizeAddr turns ":8080" into ":8080" and "0.0.0.0:8080" into ":8080" for the
// friendly log line.
func normalizeAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return addr
	}
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return addr
}
