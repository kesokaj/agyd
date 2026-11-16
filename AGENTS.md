# AGENTS.md

## Project Overview

`agy-api` exposes the Antigravity CLI (`agy`) over a local HTTP + Server-Sent Events (SSE) API by bridging HTTP requests to an `agy acp` (Agent Client Protocol v1 over stdio) child process inside a Docker container. Credentials are ingested once via `agy acp --login` into a persistent Docker volume (`/home/agy/.gemini`), and the ACP stdio seam allows swapping or adding `@agentclientprotocol/claude-agent-acp` for Claude Code later.

## Setup Commands

```bash
# Build
go build ./cmd/agy-api

# Run
go run ./cmd/agy-api

# Test
go vet ./... && go test -race ./...
```

## Architecture

`cmd/agy-api` starts a single-binary Go HTTP server bound to `127.0.0.1:8080` (or `0.0.0.0:8080` inside Docker published to host `127.0.0.1:8080`) and translates REST + SSE calls into ACP v1 JSON-RPC 2.0 messages over `stdin`/`stdout` of a managed `agy acp` subprocess (`internal/acp`). Access control is handled outside the application boundary. For ADRs: `fyr record list --kind adr`.

## Testing

Every code change under `cmd/` or `internal/` runs `go vet ./... && go test -race ./...` against deterministic stdio ACP test processes. Container image builds and live HTTP/SSE smoke checks follow `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Deploy

Local only — see `DEPLOY-local` (`fyr record get DEPLOY-local`).

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/agy-api/main.go` | Application entry point, flag/env config, signal handling |
| `internal/acp/` | ACP v1 JSON-RPC 2.0 stdio client, session registry, permission policy |
| `internal/api/` | HTTP/SSE handlers and AIP-193 JSON errors |
| `Dockerfile` | Multi-stage Go build + `debian:bookworm-slim` runtime with `agy` CLI |
| `docker-compose.yml` | Local container stack with `agy-gemini-data` volume and `/workspace` mount |
| `PRD.md` | What and why: problem, goals, the non-negotiable requirements |
| `SDD.md` | How the parts fit: components, flows, decisions |
| `TDD.md` | How each part is built and tested |
| `TODO.md` | The active phase's tasks, the next phases, the backlog |
| `.agents/` | Project customizations and `scratch/` (trail records live in `fyr`'s brain store; `fyr record export` writes `.agents/trail.jsonl` on demand) |
