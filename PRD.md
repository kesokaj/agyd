# PRD — agyd

Expose an installed `agy-cli` as a local HTTP/SSE API with Swagger docs, shipped as one host binary (`agyd`) that reuses the CLI's existing login.
Status: approved · Owner: kesokaj · Last updated: 2026-11-19 · Design: [SDD.md](SDD.md) · Technical design: [TDD.md](TDD.md)

<!-- Read before every significant change. Immutable without the user's explicit approval of the exact text:
     propose the edit → approval → ADR → PRD.md and TODO.md in one commit (global context § Hard rules).
     Nothing here needs routine edits: a goal's baseline is a dated starting point, never updated; `Last updated`
     is set by the commit that applies an approved edit; how each requirement is met and tested is traced in
     SDD.md and TDD.md, which agents keep current. The `requirements` skill elicits the items below and traces
     them; it never writes "approved" itself. -->

## Problem

Antigravity CLI (`agy`) runs as an interactive terminal process or raw stdio Agent Client Protocol (ACP) stream, which makes it awkward to call from local HTTP scripts, editors, browsers, or automation tools without writing custom JSON-RPC process management. Wrapping it in containers adds runtime friction and duplicates the `~/.gemini` login the operator already has on the host. Developers need a lightweight, local-first HTTP and Server-Sent Events (SSE) bridge that works immediately against an installed `agy` without compromising host safety.

## Users

- **Local developer / operator:** Starts `agyd` on their macOS or Linux machine, picks a default model, switches conversations and workspaces, and tests prompts interactively in Swagger UI (`/docs`).
- **Local scripts and calling tools (`curl`, Python, Node.js, editors):** Send buffered JSON or streaming SSE prompts to `/v1/prompt` and inspect or control server state via `/v1/*` or `agyd <cmd> --json`.
- **Trusted-LAN caller (opt-in):** Reaches `agyd` over a local network only when the operator explicitly opts in with `-host 0.0.0.0`.

## Goals

| # | Goal | Metric | Baseline (2026-11-16) | Target |
|---|---|---|---|---|
| G1 | Single-binary zero-container setup | Third-party Go runtime modules in `go.mod` and install commands required | Docker daemon + image build + separate OAuth volume (`ADR-001`) | 0 third-party Go runtime modules; 1-command install (`install.sh` or `go install`) reusing host `~/.gemini` |
| G2 | Exact API and OpenAPI spec parity | Share of registered `/v1/*` and `/healthz` routes verified against `internal/web/static/openapi.json` | Hand-checked routes | 100% automated match on every test run (`TestOpenAPIRoutesMatchAPI`) |
| G3 | Fast offline deterministic verification | Runtime and live-network calls of `go test -race ./...` | Manual live `agy` smoke tests | 100% pass offline (0 live `agy` calls) in under 15 s (`fyr verify`) |
| G4 | Metadata-only observability with zero content leakage | Bytes of prompt, reply, tool arguments, query string, or credential text written to structured JSON logs | Unlogged HTTP/turn/permission activity | 0 bytes leaked across HTTP, turn, lifecycle, and permission logs (`TestAccessLogAndPromptLifecycle`, `TestPermissionDecisionLogged`) |

## Non-goals

- Multi-tenant, internet-facing, or cloud-hosted deployment — `agyd` is a single-operator local host service (`ADR-002`, `ADR-004`).
- API keys, bearer tokens, or an in-app login flow — access control relies on loopback binding, Host validation, CSRF protection, and the host's existing `~/.gemini` login (`ADR-002`, `ADR-004`).
- Supporting non-`agy` ACP backends (such as `claude-agent-acp`) or a per-session backend selector — scope is `agy-cli` only (`ADR-003`).
- An interactive CLI prompt subcommand (`agyd ask`), CLI SSE client, or `/v1/sessions` route alias — prompting goes through HTTP/SSE (`POST /v1/prompt`) or `/docs`, while the CLI stays strictly control-plane (`ADR-005`, `ADR-007`).
- Acting as an OS or filesystem sandbox over `agy` — `deny_all` answers only permission requests that `agy` forwards (`ADR-005`).
- Walking `./.env` files or reading `config.env` — configuration comes only from flags, `AGYD_*` environment variables, and `state.json` (`ADR-006`).

## Must — non-negotiable

- **R1** — `agyd` ships as a single Go host binary using stdlib only (zero third-party Go runtime dependencies) that bridges HTTP/SSE to an installed `agy acp` CLI over stdio and reuses the host's `~/.gemini` login.
  <!-- provenance: operator (kesokaj), 2026-11-16; enforced by: ADR-002, ADR-003, go.mod, TestClientInitializeAndSessionLifecycle -->
- **R2** — The server binds exclusively to loopback by default (`127.0.0.1:8080`, falling back to `3000` only when no port is configured; an explicit `-port`/`AGYD_PORT` never falls back), and non-loopback binds (`-host 0.0.0.0`) require an explicit opt-in that logs a warning naming the accepted hosts.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-004, ADR-006, TestListenServeFallback, TestServeExplicitPortInUse, TestRunLogsFallbackAndNonLoopbackWarningsAtBind -->
- **R3** — Every HTTP endpoint is wrapped by Host header validation (`403 FORBIDDEN_HOST`) and Go stdlib cross-origin CSRF protection (`403 CROSS_ORIGIN_REQUEST` JSON), rejects unsupported methods with `405 METHOD_NOT_ALLOWED` + `Allow`, and caps JSON request bodies at 1 MiB (`413 BODY_TOO_LARGE`).
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-002, ADR-004, TestGuard, TestGuardExtraHosts, TestCrossOriginRefusalIsJSON, TestMuxWebRoutesMethodNotAllowed, TestErrorShapes -->
- **R4** — Every workspace (`-workspace`/`AGYD_WORKSPACE`) and conversation `cwd` must be a clean absolute path to an existing real directory outside `.gemini` (case-insensitive) with no symlinks in any path component, failing otherwise with `400 INVALID_CWD`.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-006, TestWorkspaceRejectsSymlinks, TestSessionRejectsSymlinkCwd, TestCwdRejectsSymlinks -->
- **R5** — Conversations are isolated in lazily spawned per-folder `agy acp` child processes (`exec.Cmd.Dir` = folder, pinned binary path, `AGY_API_TOKEN` stripped), deduplicated per directory and reaped when the folder's last live conversation closes.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-006, TestPerFolderProcessIsolation, TestRelativeBinaryNotHijackedByWorkspaceDir, TestConcurrentFolderProcessLifecycle, TestServerPerFolderIsolationAndStateRouting -->
- **R6** — `POST /v1/prompt` and `POST /v1/conversations/{conversationId}/prompt` wrap ACP v1 verbatim (buffered JSON always serializes `updates` as an array `[]` when empty; SSE streams `session_update` events followed by `prompt_done` omitting `updates`, or `prompt_error`) and reject concurrent prompt, patch, or delete calls on an active conversation with `409 PROMPT_IN_FLIGHT`.
  <!-- provenance: operator (kesokaj), 2026-11-19; enforced by: ADR-007, TestPromptVerbatimACPEnvelope, TestPromptSSEAndErrors, TestPatchAndDeleteDuringTurn -->
- **R7** — Configuration follows strict precedence (`flags > AGYD_* env > state.json defaults > built-in defaults`), and persistent state is stored exclusively in a single flat `state.json` under `AGYD_STATE_DIR` (default `~/.config/agyd`), written atomically (`0600` file, `0700` dir) and read non-blocking (`O_NONBLOCK`) as a regular file capped at 4 MiB.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-006, TestConfigFromEnvAndFlags, TestStateStoreFlatSchemaAndPermissions, TestStateStoreRefusesFIFOWithoutBlocking, TestStateStoreValidationAndSymlinkHardening -->
- **R8** — Structured JSON logs (`log/slog`, level controlled by `-log-level`/`AGYD_LOG_LEVEL`) record HTTP access, prompt turn metadata, conversation and child process lifecycles, and every answered `session/request_permission` decision while never logging prompt text, reply text, tool arguments, query strings, raw ACP update payloads, or credentials.
  <!-- provenance: operator (kesokaj), 2026-11-19; enforced by: TestAccessLogAndPromptLifecycle, TestPermissionDecisionLogged, TestProcessLifecycleLogs, TestConversationLifecycleLogs, TestNewLoggerLevelFiltering -->
- **R9** — The control-plane CLI (`current`, `list`, `new`, `use`, `clear`, `model`, `models`, `defaults`, `help`) maps 1:1 to `/v1/*`, rejects `ask` with exit `2`, never follows HTTP redirects, strips ANSI/VT100, raw 8-bit C1, invalid UTF-8, and Unicode format/bidi sequences from all human-readable output and the startup picker, and preserves raw API JSON under `--json`.
  <!-- provenance: operator (kesokaj), 2026-11-19; enforced by: ADR-007, ADR-008, TestCLICommandsHitTheirEndpoints, TestDispatchServeVersusClient, TestCLIDoesNotFollowRedirects, TestCLIStripsTerminalControlChars, TestPickerStripsTerminalControlChars, TestSanitizeTerminal -->
- **R10** — Embedded Swagger UI (`/docs`) and OpenAPI 3.1 (`/openapi.json`) stay 1:1 with the registered API routes, and public releases ship under the MIT License with bundled Swagger UI Apache-2.0 `LICENSE`/`NOTICE`, module path `github.com/kesokaj/agyd`, and an HTTPS-only SHA-256 checksum-verified `install.sh`.
  <!-- provenance: operator (kesokaj), 2026-11-19; enforced by: ADR-002, ADR-008, LICENSE, install.sh, internal/web/static/swagger-ui/LICENSE, internal/web/static/swagger-ui/NOTICE, TestWebRoutes, TestOpenAPIRoutesMatchAPI, TestInstallScript -->

## Should — expected unless a decision says otherwise

- **S1** — Gateway-managed clear (`POST /v1/clear`, `POST /v1/conversations/{conversationId}/clear`, `agyd clear`) creates a fresh current conversation in the same folder with the same settings while keeping the prior conversation intact in history.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-006, TestClearEndpoints, TestClearCurrentPreservesFolderAndSettings, TestServerClearReusesFolderProcAndPreservesSettings -->
- **S2** — On an interactive terminal with no model configured and `-no-prompt` unset, startup runs an interactive model and thinking-level picker with an option to save defaults to `state.json`.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-005, TestWantPicker, TestPickDefaults, TestFileIsTerminal -->
- **S3** — Every `/healthz` and `/v1/*` response includes `Agyd-Conversation`, `Agyd-Model`, `Agyd-Thinking-Level`, and `Agyd-Mode` headers (omitting empty values), and all errors return the AIP-193 JSON envelope in domain `agyd.local`.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-005, TestHeadersOnEveryResponse, TestErrorShapes, TestClassifyErrorMapping -->
- **S4** — Control-plane CLI target discovery resolves `--host`/`--port` > `AGYD_URL` > `AGYD_HOST`/`AGYD_PORT` > `/healthz` probe of `8080` then `3000`.
  <!-- provenance: operator (kesokaj), 2026-11-18; enforced by: ADR-006, TestClientBaseURL, TestCLIProbeDiscovery -->

## Could — welcome when cheap

- **C1** — Optional configurable cap on concurrent per-folder `agy acp` child processes.
- **C2** — Surface `tool_call` status from `session/update` in CLI output.
- **C3** — Side-effect-free `GET /v1/current` read that does not resume non-live conversations.

## Known limits

- `deny_all` is not a sandbox: `agy` sends `session/request_permission` only as its own `toolPermission` setting (`~/.gemini/antigravity-cli/settings.json`) permits; under `always-proceed` (observed 2026-11-18 on `agy 1.5.11`), file reads, edits, shell commands, and writes outside the workspace run without requesting permission — `ADR-005`, `DEPLOY-local`.
- Because `agyd` has no API token and shares one global current conversation pointer, binding `-host 0.0.0.0` exposes that pointer and the operator's user permissions to any device able to reach the port — `ADR-004`, `README.md`.
- Live verification targets `agy 1.5.11` with an active `~/.gemini` OAuth session; prebuilt release archives cover macOS and Linux (`arm64` and `amd64`), and Windows is not supported — `DEPLOY-local`, `install.sh`.
- Size bounds enforced by the server: 1 MiB max HTTP JSON request body (`413 BODY_TOO_LARGE`), 4 MiB max `state.json` read, 4 KiB max `acp stderr` log line (`DEBUG` only), and 64-rune cap on logged permission metadata strings — `TDD.md`.

## Open questions

- Live positive-control verification of `-permission-policy deny_all` with `agy` set to `toolPermission: request-review` (confirming `permission decision` logs and tool refusals) — tracked in `TODO.md` Next.
