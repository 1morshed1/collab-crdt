# Multi-stage build: compile a static Go binary, then ship it on a tiny base image
# together with the web/ assets. The result is a single small image with no toolchain.

# ---- build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY . .
# Dependencies are vendored into ./vendor, so the build needs NO network access — it's
# hermetic and reproducible. -mod=vendor is auto-selected when a vendor/ dir is present.
# CGO disabled => a self-contained static binary that runs on alpine/scratch.
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage ----
FROM alpine:3.20
WORKDIR /app
# The server serves static files from ./web at runtime (STATIC_DIR defaults to "web").
COPY --from=build /out/server /app/server
COPY web /app/web

EXPOSE 8080
ENV LISTEN_ADDR=:8080 STATIC_DIR=/app/web
ENTRYPOINT ["/app/server"]
