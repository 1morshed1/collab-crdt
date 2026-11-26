# 07 — Running it, configuring it, operating it

> Code: `cmd/server/main.go`, `Dockerfile`, `docker-compose.yml`, `Makefile`,
> `.env.example`.

This doc is the practical "how do I actually run this" guide, plus the reasoning behind
the operational choices.

## The fastest possible start (no dependencies)

```bash
make run-mem          # or: go run ./cmd/server
# open http://localhost:8080/?doc=notes in two tabs
```

With no database and no Redis, the server keeps documents **in memory only** and runs as a
**single instance**. That's enough to type in two tabs and watch convergence. It's the
zero-friction path for trying it out, and it exists because the entry point degrades gracefully
(see below).

## The full stack

```bash
make up               # docker compose: builds the app, starts Postgres + Redis, serves :8080
```

or run the infra in Docker and the app on your host (nice for fast iteration):

```bash
make infra-up         # Postgres on :5432, Redis on :6379
make run              # server on :8080, wired to both
```

## Configuration: everything via environment variables

`cmd/server/main.go` reads only environment variables — there's no config file to manage.
Every one is optional:

| Var | Default | Effect |
|-----|---------|--------|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `STATIC_DIR` | `web` | Folder served at `/` |
| `DATABASE_URL` | _(unset)_ | Set → PostgreSQL persistence on. Unset → in-memory. |
| `REDIS_ADDR` | _(unset)_ | Set → Redis cross-pod fan-out on. Unset → single instance. |
| `INSTANCE_ID` | random | This pod's id, for cross-pod echo suppression |

Copy `.env.example` to `.env` for reference. The design principle here: **graceful
degradation**. Each external dependency is an enhancement, not a requirement, so the
barrier to running the project is just "have Go" — and each layer can be turned on
independently to study it in isolation.

## Seeing multi-pod collaboration for real

```bash
make up-multi         # two app pods: :8080 and :8081, sharing one Redis + one Postgres
```

Open `http://localhost:8080/?doc=x` and `http://localhost:8081/?doc=x` in two tabs. You're
now connected to **different server processes**, yet edits sync — that's the Redis bus
(docs/05) doing its job. This is the tangible payoff of the `Bus` seam.

## How the pieces are wired (`main.go`)

```
read env
 ├─ if DATABASE_URL: store.Open() → applies migrations → server.Store
 ├─ if REDIS_ADDR:   presence.NewRedisBus() → server.Bus
 ├─ hub := server.NewHub(store, bus, instanceID)   // both may be nil
 ├─ srv := server.New(hub, staticDir)
 └─ http.ListenAndServe(...) with graceful shutdown on SIGINT/SIGTERM
```

Note the `Store` and `Bus` are just interfaces (docs/03). `main.go` is the *only* place
that knows about concrete Postgres and Redis; the server package never imports them. That's
what keeps the core testable without infrastructure.

## The container image

`Dockerfile` is a two-stage build:

1. **build stage** compiles a static binary (`CGO_ENABLED=0`) using the **vendored**
   dependencies in `./vendor`, so the build needs **no network** — it's hermetic and
   reproducible (and it builds fine behind restrictive proxies).
2. **runtime stage** is a tiny Alpine image carrying just the binary and the `web/` assets.

Migrations ship *inside* the binary (docs/04), so the container needs nothing but a
reachable Postgres to bootstrap its schema on first boot.

> On vendoring: we ran `go mod vendor` and commit `vendor/`. It costs some repo size but
> buys hermetic, offline, proxy-independent builds — a good trade for a project people will
> clone and build in varied environments. Keep it in sync with `go mod tidy && go mod
> vendor` when dependencies change.

## Operating notes

- **Graceful shutdown:** SIGINT/SIGTERM triggers `http.Server.Shutdown`, letting in-flight
  requests finish. WebSocket clients simply reconnect (docs/06), so a rolling restart is
  seamless — a client's fresh `init` resyncs it.
- **Where state lives:** durable state is only in Postgres. Rooms are in-memory caches that
  reload from Postgres when reopened (docs/03/04). So losing a pod loses no data.
- **Health check:** `GET /healthz` returns `ok` (used by the compose healthchecks and any
  load balancer).
- **Scaling:** add pods (`up-multi` shows two). All pods share Postgres + Redis. There's no
  sticky-session requirement because the Redis bus bridges pods — though sticky sessions
  would reduce cross-pod chatter.

Next: `docs/08-testing-and-load.md` — how we test correctness and measure performance.
