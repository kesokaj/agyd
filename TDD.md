# TDD — agy-api

Technical design for `cmd/agy-api`, `internal/acp`, and `internal/api`, linking each component to `SPEC-agy-api-acp-gateway` and its verification tests.
Status: draft · Last updated: 2026-11-16 · Requirements: [PRD.md](PRD.md) · Design: [SDD.md](SDD.md)

## T1 — `cmd/agy-api` (Entry Point & Process Wiring)

Implements: M1 · Spec: `SPEC-agy-api-acp-gateway`

Wires CLI flags and environment variables, starts the `internal/acp` client, mounts the `internal/api` HTTP mux, and performs graceful shutdown on `SIGINT`/`SIGTERM`.

- **Interfaces:** CLI binary `agy-api` accepting flags (`-addr`, `-acp-bin`, `-acp-args`, `-workspace`, `-permission-policy`) and environment overrides (`PORT`, `AGY_API_ADDR`, `AGY_API_TOKEN`, `AGY_ACP_BIN`, `AGY_ACP_ARGS`, `AGY_WORKSPACE`, `AGY_PERMISSION_POLICY`).
- **Errors:** Exits non-zero (`1`) with a JSON `ERROR` log line if configuration validation fails (such as missing `AGY_API_TOKEN` when `-allow-unauthenticated` is false) or the listener fails to bind.
- **Configuration:** All runtime paths (`/workspace`, `~/.gemini`) live outside the repository tree.

## T2 — `internal/acp` (ACP v1 JSON-RPC 2.0 Stdio Client)

Implements: M2 · Spec: `SPEC-agy-api-acp-gateway`

Manages the child ACP agent process (`agy acp` or `claude-agent-acp`) over `stdin`/`stdout` NDJSON-RPC 2.0, multiplexing concurrent RPC calls, streaming `session/update` notifications to active session subscribers, and answering agent-initiated `session/request_permission` requests.

- **Interfaces:** `Client` (`Start`, `Close`, `Status`, `Authenticate`, `NewSession`, `LoadSession`, `ListSessions`, `CloseSession`, `SetMode`, `SetConfigOption`, `Prompt`, `Cancel`).
- **Errors:** Translates JSON-RPC error objects (`code`, `message`, `data`), context cancellations, and unexpected child exits into typed Go errors (`*RPCError`, `ErrProcessClosed`, `ErrSessionNotFound`, `ErrPromptInFlight`).
- **Configuration:** Configurable binary path, arguments, working directory, and permission policy (`allow_all` vs `deny_all`).

## T3 — `internal/api` (REST + SSE HTTP Gateway)

Implements: M3 · Spec: `SPEC-agy-api-acp-gateway`

Serves the HTTP API (`GET /healthz`, `GET /v1/status`, `POST /v1/auth/login`, `POST /v1/sessions`, `GET /v1/sessions`, `GET /v1/sessions/{id}`, `POST /v1/sessions/{id}/load`, `POST /v1/sessions/{id}/prompt`, `POST /v1/sessions/{id}/cancel`, `POST /v1/sessions/{id}/mode`, `POST /v1/sessions/{id}/config`, `DELETE /v1/sessions/{id}`) with constant-time bearer authentication and AIP-193 error responses.

- **Interfaces:** `NewHandler(log, acpClient, cfg) http.Handler`; supports both `Accept: text/event-stream` (SSE streaming `session_update`, `prompt_done`, `prompt_error`) and `Accept: application/json` (buffered response with collected updates and final text).
- **Errors:** AIP-193 JSON error envelope (`{"error":{"code":...,"status":...,"message":...,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":...,"domain":"agy-api.local"}]}}`).
- **Configuration:** Bearer token string (`AGY_API_TOKEN`), default workspace directory (`AGY_WORKSPACE`), and request body size limit (`1 MiB`).

## Data model

Session metadata (`sessionId`, `cwd`, `backend`, `createTime`, `modes`, `models`, `configOptions`) is held in-memory in `internal/acp` and persisted on disk by `agy acp` inside the mounted `/home/agy/.gemini` volume; `agy-api` owns no database.

## Testing strategy

See `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Build, run and deploy

See the `DEPLOY-local` record (`fyr record get DEPLOY-local`).

## Trace

Every component in `SDD.md` maps to its technical design section and test suite.

| Component | Section | Verified by |
|---|---|---|
| M1 | T1 | `cmd/agy-api` build + `TestConfigFromEnvAndFlags` |
| M2 | T2 | `TestClientInitializeAndSessionLifecycle`, `TestClientPromptStreamingAndPermission`, `TestClientCancelAndCrashRecovery` |
| M3 | T3 | `TestAuthMiddleware`, `TestSessionEndpoints`, `TestPromptSSEAndJSON`, `TestCancelEndpoint` |
