# AGENTS.md

## Project Overview

`agyd` exposes an installed Antigravity CLI (`agy`) over a local HTTP + Server-Sent Events (SSE) API with interactive Swagger docs by bridging HTTP requests to an `agy acp` (Agent Client Protocol v1 over stdio) child process on the host. It reuses the operator's existing `~/.gemini` login, and the ACP stdio seam allows swapping or adding `@agentclientprotocol/claude-agent-acp` for Claude Code later.

## Setup Commands

```bash
# Build
go build ./cmd/agyd

# Run
go run ./cmd/agyd

# Test
go vet ./... && go test -race ./...
```

## Architecture

`cmd/agyd` starts a single-binary Go HTTP server bound strictly to loopback (`127.0.0.1:8080` by default) with Host header validation and stdlib `http.CrossOriginProtection` (CSRF). It serves OpenAPI 3.1 Swagger UI documentation (`internal/web`) and translates REST + SSE calls into ACP v1 JSON-RPC 2.0 messages over `stdin`/`stdout` of a managed `agy acp` subprocess (`internal/acp`). Access control is enforced via loopback binding, Host guard, and CSRF protection (no API token). For ADRs: `fyr record list --kind adr`.

## Testing

Every code change under `cmd/` or `internal/` runs `go vet ./... && go test -race ./...` against deterministic stdio ACP test processes and HTTP mocks. Live host smoke checks and Swagger UI validation follow `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Deploy

Local only — see `DEPLOY-local` (`fyr record get DEPLOY-local`).

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/agyd/` | Application entry point, flag/env config, mux composition, signal handling |
| `internal/acp/` | ACP v1 JSON-RPC 2.0 stdio client, session registry, permission policy |
| `internal/api/` | HTTP/SSE handlers, route table, Host/CSRF guard, and AIP-193 JSON errors |
| `internal/web/` | Embedded Swagger UI assets, `docs.html`, and `openapi.json` spec |
| `PRD.md` | What and why: problem, goals, the non-negotiable requirements |
| `SDD.md` | How the parts fit: components, flows, decisions |
| `TDD.md` | How each part is built and tested |
| `TODO.md` | The active phase's tasks, the next phases, the backlog |
| `.agents/` | Project customizations and `scratch/` (trail records live in `fyr`'s brain store; `fyr record export` writes `.agents/trail.jsonl` on demand) |
