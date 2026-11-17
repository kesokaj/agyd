# TDD — agy-api

Technical design for `cmd/agyd`, `internal/acp`, `internal/api`, and `internal/web`, linking each component to `SPEC-agy-api-acp-gateway` and its verification tests.
Status: agreed · Last updated: 2026-11-16 · Requirements: [PRD.md](PRD.md) · Design: [SDD.md](SDD.md)

## T1 — `cmd/agyd` (Entry Point, Config, & Process Wiring)

Implements: M1 · Spec: `SPEC-agy-api-acp-gateway`

Wires CLI flags and environment variables, validates loopback binding and host binary availability, starts the `internal/acp` client, composes `internal/web` and `internal/api` routes wrapped with `api.Guard`, and executes graceful shutdown on `SIGINT`/`SIGTERM`.

- **Interfaces:** CLI binary `agyd` accepting flags (`-addr`, `-agy`, `-acp-args`, `-workspace`, `-permission-policy`) and environment overrides (`AGYD_ADDR`, `AGYD_AGY`, `AGYD_ACP_ARGS`, `AGYD_WORKSPACE`, `AGYD_PERMISSION_POLICY`).
- **Errors:** Exits `1` with a JSON `ERROR` log line if configuration validation fails (non-loopback `-addr`, invalid `-permission-policy`, positional arguments, or missing `agy` executable on `$PATH`).
- **Configuration:** Workspace defaults to `os.Getwd()`. Address defaults to `127.0.0.1:8080`.

## T2 — `internal/acp` (ACP v1 JSON-RPC 2.0 Stdio Client)

Implements: M2 · Spec: `SPEC-agy-api-acp-gateway`

Manages the child ACP agent process (`agy acp`) over `stdin`/`stdout` NDJSON-RPC 2.0, multiplexing concurrent RPC calls, streaming `session/update` notifications to active session subscribers, answering agent-initiated `session/request_permission` requests, and lazily respawning the child process if it exits unexpectedly.

- **Interfaces:** `Client` (`Start`, `Close`, `Status`, `NewSession`, `GetSession`, `ListSessions`, `CloseSession`, `Prompt`, `Cancel`).
- **Errors:** Translates JSON-RPC error objects (`code`, `message`, `data`), context cancellations, and unexpected child exits into typed Go errors (`*RPCError`, `ErrProcessClosed`, `ErrSessionNotFound`, `ErrPromptInFlight`).
- **Configuration:** Configurable binary path, arguments, working directory, and permission policy (`ALLOW_ALL` vs `DENY_ALL`).

## T3 — `internal/api` (REST + SSE HTTP Gateway & Security Guard)

Implements: M3 · Spec: `SPEC-agy-api-acp-gateway`

Serves the HTTP API (`GET /healthz`, `GET /v1/status`, `POST /v1/sessions`, `GET /v1/sessions`, `GET /v1/sessions/{sessionId}`, `POST /v1/sessions/{sessionId}/prompt`, `POST /v1/sessions/{sessionId}/cancel`, `DELETE /v1/sessions/{sessionId}`) with AIP-193 error responses. Defines the route table and security middleware.

- **Interfaces:**
  - `NewHandler(log, acpClient, cfg) http.Handler`: mounts API routes and handles both `Accept: text/event-stream` and `Accept: application/json`.
  - `Guard(next http.Handler) http.Handler`: validates Host headers against loopback targets (`localhost`, `127.0.0.1`, `::1`) returning `403 FORBIDDEN_HOST` on mismatch, and applies `http.CrossOriginProtection` against cross-site requests.
  - `Routes []Route`: exported list of registered API routes tested against `openapi.json`.
- **Errors:** AIP-193 JSON error envelope (`{"error":{"code":...,"status":...,"message":...,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":...,"domain":"agyd.local"}]}}`).
- **Configuration:** Default workspace directory (`AGYD_WORKSPACE`) and request body size limit (`1 MiB`).

## T4 — `internal/web` (Swagger UI & OpenAPI Documentation)

Implements: M4 · Spec: `SPEC-agy-api-acp-gateway`

Serves the vendored Swagger UI static distribution (pinned version 5.35.0) and hand-written OpenAPI 3.1 specification embedded via `//go:embed`.

- **Interfaces:** `Register(mux *http.ServeMux)` mounts `GET /{$}` (302 redirect to `/docs`), `GET /docs` (Swagger UI HTML), `GET /openapi.json` (OpenAPI specification), and `GET /static/*` (vendored CSS/JS assets).
- **Security headers:** Injects `Content-Security-Policy: default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'` and `X-Content-Type-Options: nosniff`. Zero inline scripts; Swagger initialization runs from `docs.js`.

## Data model

Session metadata (`sessionId`, `cwd`, `backend`, `createTime`, `modes`, `models`, `configOptions`) is held in-memory in `internal/acp` and persisted on disk by `agy acp` inside host `~/.gemini`; `agyd` owns no database.

## Testing strategy

See `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Build, run and deploy

See the `DEPLOY-local` record (`fyr record get DEPLOY-local`).

## Trace

Every component in `SDD.md` maps to its technical design section and test suite.

| Component | Section | Verified by |
|---|---|---|
| M1 | T1 | `cmd/agyd` build, `TestConfigFromEnvAndFlags`, `TestMuxUnknownPathJSON` |
| M2 | T2 | `TestClientInitializeAndSessionLifecycle`, `TestClientPromptStreamingAndPermission`, `TestClientCancelAndCrashRecovery` |
| M3 | T3 | `TestStatusAndHealthEndpoints`, `TestSessionEndpoints`, `TestPromptSSEAndJSON`, `TestCancelEndpoint`, `TestGuard` |
| M4 | T4 | `TestWebRoutes`, `TestOpenAPISpecMatchesRoutes` |
