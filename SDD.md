# SDD — agyd

A single Go binary (`cmd/agyd`) serves a minimal REST/SSE API that wraps Agent Client Protocol (ACP v1) JSON-RPC 2.0 messages verbatim over stdio to per-folder `agy acp` subprocesses, keeps one current conversation with model/thinking defaults, serves embedded Swagger UI, and provides a thin control-plane CLI (`current`, `list`, `new`, `use`, `clear`, `model`, `models`, `defaults`, `help`) backed 1:1 by `/v1/*`.
Status: agreed · Last updated: 2026-11-19 · Requirements: [PRD.md](PRD.md) · Technical design: [TDD.md](TDD.md)

## Context

`agyd` runs directly on the operator's machine, listening on `127.0.0.1:8080` (falling back to `3000` when `8080` is in use and no port was set). It supervises the host's installed `agy` CLI in `acp` mode, inheriting the operator's existing `~/.gemini` login without credentials in transit or containers. Its own state is a single `state.json` file in the state directory (`AGYD_STATE_DIR`, default `~/.config/agyd`).

```mermaid
flowchart LR
  CLI["agyd current / new / use ...\n(M7 control plane)"] -->|"HTTP"| Guard
  Caller["curl / Swagger UI / HTTP clients"] -->|"HTTP + SSE"| Guard["api.Guard\nHost Check + CSRF"]
  Guard --> M1["M1: cmd/agyd\nServe + Mux"]
  M1 --> M4["M4: internal/web\nSwagger UI + openapi.json"]
  M1 --> M3["M3: internal/api\nREST and SSE Endpoints"]
  M3 --> M6["M6: internal/conversation\nCurrent pointer + settings"]
  M6 --> M2["M2: internal/acp\nJSON-RPC 2.0 Stdio Client"]
  M6 --> M5["M5: internal/config\nstate.json"]
  M2 -->|"ACP v1 NDJSON-RPC\nstdin / stdout"| Agy["Per-folder agy acp\n(exec.Cmd.Dir = folder,\nreusing ~/.gemini login)"]
  Agy -.->|"Read / Write / Shell"| Workspace["Workspace Directory\n(cwd)"]
```

## Components

Every package has one responsibility across the HTTP/SSE gateway, per-folder ACP supervision, flat state store, and thin control-plane CLI.

| ID | Component | Responsibility | Satisfies |
|---|---|---|---|
| M1 | `cmd/agyd` (serve) | Entry point and first-word dispatch (serve vs control-plane CLI), flag and `AGYD_*` env config with `-state-dir` support (`serveconfig.go`), `8080` -> `3000` listen fallback (`listen.go`), Host allowlist for a non-loopback bind, startup conversation and model picker (`startup.go`, `picker.go`), mux composition wrapped in `api.Guard`, `log/slog`, graceful `SIGINT`/`SIGTERM` shutdown | — |
| M2 | `internal/acp` | Per-folder `agy acp` process pool (`exec.Cmd.Dir` = folder, `os.SameFile` deduplication, pinned binary path, synchronous reap when last conversation closes), ACP v1 JSON-RPC 2.0 stdio transport, handshake, session lifecycle, `session/set_config_option`, `session/list`, `session/resume`, process-scoped `session/update` fan-out, `session/request_permission` policy | — |
| M3 | `internal/api` | REST and SSE handlers on the Manager, strict verbatim ACP v1 prompt envelope (buffered JSON always serializes `updates` array; SSE omits `updates` on `prompt_done`), route table, `Agyd-*` response headers, Host validation and `http.CrossOriginProtection` (`api.Guard`), AIP-193 errors | — |
| M4 | `internal/web` | Embedded OpenAPI 3.1 spec (`openapi.json`), vendored Swagger UI, root redirect, CSP/nosniff headers | — |
| M5 | `internal/config` | Flat `state.json` store (`currentConversationId`, `defaults`, `conversations`), `AGYD_STATE_DIR` resolution, safe non-blocking regular file reading (capped at 4 MiB), `ValidSettingValue`, atomic 0600 writes | — |
| M6 | `internal/conversation` | `Manager`: the one owner of current pointer, defaults, per-conversation memo settings, and full `state.json` snapshots under `saveMu` (lock order: `opMu -> saveMu -> mu`); startup flag/env defaults override without clobbering disk; resolve-or-create, clear (fresh conversation with same folder and settings), use/resume, list merge with `agy` history | — |
| M7 | `cmd/agyd` (control-plane CLI) | `current|list|new|use|clear|model|models|defaults|help`: thin stdlib HTTP control plane backed 1:1 by `/v1/*` (no `ask` subcommand, no SSE client), base URL from `--host`/`--port` > `AGYD_URL` > `AGYD_HOST`/`AGYD_PORT` > `/healthz` probe of `8080` then `3000`, readable text/table output by default, stderr `Agyd-*` context line, `--json` for scripts | — |

## Route table

All HTTP routes are exposed through the outer mux wrapped in `api.Guard` (ADR-005 renamed `/v1/sessions` to `/v1/conversations`).

| Method & Path | Package | Purpose |
|---|---|---|
| `GET`, `HEAD /{$}` | `internal/web` | 302 redirect to `/docs` |
| `GET`, `HEAD /docs`, `/openapi.json`, `/static/*` | `internal/web` | Swagger UI, spec, and assets (joined to the method table: wrong methods answer `405 METHOD_NOT_ALLOWED` with `Allow: GET, HEAD`) |
| `GET /healthz` | `internal/api` | Liveness and service identity: `200 {"service":"agyd"}` |
| `GET /v1/status` | `internal/api` | ACP child readiness, backend info, permission policy, default `workspace` |
| `GET`, `PATCH /v1/current` | `internal/api` | Current conversation; change its settings |
| `POST /v1/prompt`, `POST /v1/cancel`, `POST /v1/clear` | `internal/api` | Turn on the current conversation (auto-creates one; verbatim ACP v1 buffered JSON or SSE); cancel it; clear into a fresh conversation with the same folder and settings (`201`) |
| `POST`, `GET /v1/conversations` | `internal/api` | Create (becomes current unless `use:false`); list live plus `agy` history |
| `GET`, `PATCH`, `DELETE /v1/conversations/{conversationId}` | `internal/api` | One conversation; change settings; close |
| `POST /v1/conversations/{conversationId}/prompt`, `/cancel`, `/use`, `/clear` | `internal/api` | Turn (verbatim ACP v1 buffered JSON or SSE), cancel, make current (resumes if not live), clear into a fresh current conversation keeping the old one in history (`201`) |
| `GET /v1/models` | `internal/api` | Models, thinking levels, modes from the latest configOptions |
| `GET`, `PUT /v1/defaults` | `internal/api` | Defaults for new conversations; PUT persists to `state.json` |

`api.Routes()` returns the registered operations; a unit test asserts they match the path-method matrix in `internal/web/static/openapi.json`.

## Flows

The diagrams below trace server startup and how `POST /v1/prompt` bridges HTTP callers (both buffered JSON and SSE) to a per-folder `agy acp` subprocess.

### Startup

```mermaid
sequenceDiagram
  participant M as M1 serve
  participant C as M5 config
  participant G as M6 Manager
  participant A as M2 acp
  M->>C: Resolve flags, AGYD_* env, -state-dir
  M->>M: listen on host:port (8080 -> 3000 unless explicit)
  M->>G: Startup (60 s)
  G->>C: Load state.json (flat snapshot)
  alt saved conversation
    G->>A: session/resume, then re-apply its settings
  else none or resume failed (warn)
    G->>A: session/new, then apply defaults (lenient)
  end
  opt TTY and no flag/env/saved default sets a model
    M->>M: picker: model, thinking, save to state.json?
  end
  M->>M: serve HTTP requests
```

### `POST /v1/prompt` on the current conversation

```mermaid
sequenceDiagram
  participant U as Client (curl / app)
  participant API as M3 api (behind Guard)
  participant G as M6 Manager
  participant A as M2 acp
  participant X as agy acp
  U->>API: POST /v1/prompt (JSON or Accept: text/event-stream)
  API->>G: Resolve("") once (create if empty, resume if not live)
  G-->>API: conversation id + settings
  API->>A: Prompt(id, blocks, onUpdate)
  A->>X: session/prompt
  alt Accept: text/event-stream (SSE)
    loop each ACP notification
      X-->>A: session/update
      API-->>U: event: session_update (verbatim ACP object)
    end
    X-->>A: result {stopReason}
    API-->>U: event: prompt_done (omits updates)
  else Buffered JSON (default)
    X-->>A: session/update ...
    X-->>A: result {stopReason}
    API-->>U: 200 JSON (Agyd-* headers, verbatim text, updates array — [] if empty)
  end
```

A model change (`PATCH /v1/current`, `agyd model`) marks the conversation busy, sends `session/set_config_option` per changed field, and replaces the stored configOptions with `agy`'s reply; a turn in flight makes it `409 PROMPT_IN_FLIGHT`.

## Decisions

Architectural choices link to their ADR records (`fyr record get ADR-NNN`).

- Ship `agyd` as a single host binary wrapping installed `agy` CLI over stdio ACP, protected by loopback binding, Host header guard, and CSRF protection, reusing the host `~/.gemini` login — `ADR-002` (supersedes `ADR-001`'s Docker runtime).
- Scope to `agy-cli` only; other ACP backends deferred — `ADR-003`.
- Default to loopback but allow `-host 0.0.0.0` as an explicit, warned opt-in, with the Host guard extended to the machine's own IPs and hostname — `ADR-004` (amends `ADR-002`).
- Rename `/v1/sessions` to `/v1/conversations` (no alias), add one current conversation with model/thinking/mode settings and saved defaults, layered config with a `./.env` allowlist, `session/resume`-only restarts, and the `agyd` CLI client in the same binary — rejected: a `/v1/sessions` alias, `session/load`, unrestricted `./.env`, a separate client binary — `ADR-005` (amended by `ADR-006`, `ADR-007`).
- Eliminate `./.env` walking and `config.env` in favour of direct flags > `AGYD_*` environment variables > saved defaults in `state.json` > built-in defaults; replace per-port instances in `state.json` with a single flat snapshot (`currentConversationId`, `defaults`, `conversations`) located via `-state-dir`/`AGYD_STATE_DIR` (default `~/.config/agyd`), written atomically (0600) and read non-blocking (`O_NONBLOCK`, 4 MiB cap); reject symlinks anywhere in `-workspace`/`AGYD_WORKSPACE` or conversation `cwd` (`400 INVALID_CWD`, naming the real path); isolate conversations by running one `agy acp` child process per folder (`exec.Cmd.Dir` = folder, started on first conversation there, stopped when its last conversation closes) routed from `state.json` (falling back to `session/list` `cwd` or default workspace); expose gateway-managed clear (`POST /v1/clear`, `POST /v1/conversations/{conversationId}/clear`, `agyd clear [<id>]`) that creates a new current conversation with the source's folder and settings while leaving the old conversation intact in history (never forwarding literal `"/clear"` to `agy`); lock hierarchy `opMu -> saveMu -> mu` (and `Client.mu -> folderProc.mu`); bind `8080` then `3000` when no port is set (explicit `-port`/`AGYD_PORT` never falls back), return `200 {"service":"agyd"}` on `GET /healthz`, and resolve client base URL via `--host`/`--port` > `AGYD_URL` > `AGYD_HOST`/`AGYD_PORT` > `/healthz` probe of `8080` then `3000` — `ADR-006` (amends `ADR-005` and `ADR-002`).
- Drop the interactive `ask` subcommand from the CLI (exiting `2` as an unknown command), keep the CLI strictly as server + thin control plane backed 1:1 by `/v1/*` (`current`, `list`, `new`, `use`, `clear`, `model`, `models`, `defaults`, `help`) with readable text/table output by default and `--json` for scripts, and tighten `POST /v1/prompt` and `POST /v1/conversations/{conversationId}/prompt` as a strict verbatim ACP v1 wrapper (buffered JSON always serializes `updates` as a JSON array `[]` when empty; SSE streams verbatim `session_update` events and omits `updates` on `prompt_done`; no custom `output.items` taxonomy and no stripping or rewriting of `agy` text) — `ADR-007` (amends `ADR-005`).

## Cross-cutting

Security, privacy, observability, and cost boundaries apply uniformly across every HTTP handler, ACP child process, and control-plane CLI command.

- **Security:** Loopback by default; `-host 0.0.0.0` is a warned opt-in (`ADR-004`). `api.Guard` enforces Host matching (`403 FORBIDDEN_HOST`) and `http.CrossOriginProtection` (`403 CROSS_ORIGIN_REQUEST`, JSON). No API token. Tool calls follow `allow_all` (default) or `deny_all`, which only answers the permission requests `agy` sends (not a sandbox, `ADR-005`). `-workspace`/`AGYD_WORKSPACE` and every conversation `cwd` (create and history resume) must be clean absolute paths to existing real directories outside `.gemini` (case-insensitive) with no symlinks in any component; symlinks to directories outside `.gemini` fail naming the real path, while non-directory and `.gemini` targets are never disclosed (`ADR-006`). No `./.env` walking or `config.env` parsing is performed (`ADR-006`). `state.json` is opened non-blocking (`O_NONBLOCK`) as a regular file of at most 4 MiB. `PUT /v1/defaults` validates setting values matching `^[A-Za-z0-9._-]{1,64}$`, writing `state.json` atomically (mode 0600 file, 0700 dir) serialized under `saveMu`. The CLI never reads env files and never follows redirects.
- **Privacy:** Request bodies, prompts, and credentials are never logged. `GET /v1/conversations` exposes `agy`'s conversation titles to any caller that passes the guard (`ADR-005`).
- **Observability:** Structured JSON logs via `log/slog` (`severity`, `message`, `timestamp`); one `permission decision` line per `session/request_permission` `agyd` answers and one `unsupported agent request` line per other agent request (ids, kinds, and outcome only; no tool titles); `Agyd-Conversation`, `Agyd-Model`, `Agyd-Thinking-Level`, `Agyd-Mode` headers on every `/healthz` and `/v1/*` response; `GET /healthz` and `GET /v1/status` report readiness.
- **Cost:** Local host binary only; zero cloud cost.

## Deployment

`agyd` runs locally on the host machine (`go build -o bin/ ./cmd/agyd`, then `bin/agyd`); see `DEPLOY-local` (`fyr record get DEPLOY-local`).

## Risks

Operational failure modes and their mitigations center on host `agy` availability, OAuth state, and the shared current conversation pointer.

- `agy` not on `$PATH` — detected at startup via `exec.LookPath`; `agyd` exits `1` with `agy not found on PATH; install agy-cli or pass -agy /path/to/agy`.
- `agy acp` exits mid-turn — the stdio reader fails pending calls, clears the registry, and restarts lazily; the next request on the current pointer resumes it.
- OAuth expiry in `~/.gemini` — ACP auth errors map to `401 ACP_AUTH_REQUIRED`; run `agy` interactively once.
- `agy` drops a conversation from its history — startup resume fails and `agyd` creates a fresh conversation with a warning.
- The current pointer is global — every client (and the LAN on a `0.0.0.0` bind) shares it; responses name the conversation used and scripts can address IDs explicitly.

## Trace

Requirement-to-component mapping stays `—` until `PRD.md` requirements are approved via `/fyr-requirements`.

| Requirement | Components |
|---|---|
| — | M1, M2, M3, M4, M5, M6, M7 |
