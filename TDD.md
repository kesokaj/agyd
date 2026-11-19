# TDD — agyd

Technical design for `cmd/agyd` (serve and control-plane CLI), `internal/acp`, `internal/api`, `internal/web`, `internal/config`, and `internal/conversation`, linking each component to its verification tests.
Status: agreed · Last updated: 2026-11-19 · Requirements: [PRD.md](PRD.md) · Design: [SDD.md](SDD.md)

## T1 — `cmd/agyd` serve (entry point, config, startup)

Implements: M1 · Decisions: `ADR-004`, `ADR-005`, `ADR-006`

The first argument picks the mode: none, a flag, or `serve` starts the server; a known control-plane command runs the CLI (T7). Serve resolves settings through flags and `AGYD_*` env vars, falling back to saved defaults in `state.json` (for model, thinking, mode), then built-in defaults. It validates settings, binds the listen socket (`8080` then `3000` when unset; an explicit `-port` or `AGYD_PORT` never falls back), initializes the lazy per-folder ACP client without eagerly spawning a process, runs `Manager.Startup` (60 s), composes `internal/web` and `internal/api` under `api.Guard`, and shuts down all folder children on `SIGINT`/`SIGTERM`.

- **Interfaces:** flags `-host`, `-port`, `-workspace`, `-state-dir`, `-permission-policy`, `-log-level`, `-agy`, `-acp-args`, `-model`, `-thinking`, `-mode`, `-no-prompt`, `-h`; each has an `AGYD_*` env twin. Precedence: flags > `AGYD_*` env vars > saved defaults in `state.json` > built-in defaults. `parseLogLevel(raw)` parses `debug`, `info`, `warn`/`warning`, or `error` case-insensitively; invalid levels exit 1 with `"configuration error"`. `newLogger(w, level)` constructs a JSON `log/slog.Logger` mapping `level` to `severity` (rendering `WARN` as `WARNING`), `msg` to `message`, and `time` to `timestamp`, controlled dynamically via `slog.LevelVar`.
- **Startup picker** (`picker.go`, `startup.go`, `tty_*.go`): runs only on a real terminal (`TIOCGETA`/`TIOCGWINSZ` ioctl, never `/dev/null`), with agy ready, when no flag, env var, or saved default set a model and `-no-prompt` is off. It offers agy's models and thinking levels and, on "save", writes them to `state.json` defaults via Manager.
- **Errors:** exit `1` with a JSON `ERROR` line on invalid host/port/policy/state-dir/workspace/log-level (including missing directories, `.gemini`, or any symlink in `-workspace`/`AGYD_WORKSPACE`, naming the real path), unknown flags, positional arguments, a missing `agy` on `$PATH`, or an in-use port (naming both `8080` and `3000` plus `-port` when both default ports are held). A non-loopback `-host` logs a `WARNING` naming the accepted hosts.

## T2 — `internal/acp` (ACP v1 JSON-RPC 2.0 stdio client)

Implements: M2 · Decisions: `ADR-002`, `ADR-005`, `ADR-006`

Supervises one `agy acp` subprocess (`folderProc` in `proc.go`) per workspace folder over NDJSON-RPC 2.0 (`exec.Cmd.Dir` = folder, `PWD` set to folder, `AGY_API_TOKEN` stripped, binary path pinned to an absolute path in `NewClient` before any `cmd.Dir` is used). Each folder process starts lazily on its first conversation (`NewSession`/`ResumeSession`), is deduplicated across case variants via `os.SameFile`, shares one child across concurrent conversations in the same folder, and is stopped and reaped (`cmd.Wait()`) when its last live conversation closes or fails. `Status` and `ListAgentSessions` reuse any live folder process without spawning an extra child (falling back to a short-lived probe in `DefaultWorkspace` when none are live). Incoming `session/update` notifications are accepted only from the owning folder process (`entry.proc == p`). Lock order: `Client.mu -> folderProc.mu` (`folderProc.mu` is a leaf lock; `stop()` runs outside locks).

- **Interfaces:** `NewClient`, `Start`, `Status`, `NewSession`, `GetSession`, `ListSessions`, `CloseSession`, `Prompt`, `Cancel`, `SetConfigOption` (`session/set_config_option`), `ListAgentSessions` (`session/list`, agy's history), `ResumeSession` (`session/resume`; settings reset to agy defaults), `Close`. `procPID(cmd)` extracts child PID nil-safely. Process lifecycle emits `acp process started` (`dir`, `pid`, `agent`, `version`), `acp process stopped` on expected shutdown, and `acp process exited unexpectedly` at `WARN` (`dir`, `pid`, `err`) on crashes; `drainStderr` streams `acp stderr` lines at `DEBUG`.
- **Errors:** `*RPCError`, `ErrProcessClosed`, `ErrSessionNotFound`, `ErrPromptInFlight` (also for set-config and close during a turn), `ErrInvalidArgument` (`-32602`, e.g. an unknown model), `ErrInvalidConfig`.
- **Permission policy:** `ALLOW_ALL` picks the first allow option, `DENY_ALL` the first reject option, else cancels. It applies only to requests agy sends (`ADR-005` known limit); agy sends none while its own `toolPermission` is `always-proceed`.
- **Permission log:** every `session/request_permission` answer writes one INFO line `permission decision` (`conversationId`, `toolCallId`, `toolKind`, the first 8 `options` as `optionId`/`kind`, `optionCount`, `policy`, `outcome`, `optionId`, `optionKind`, `err` on malformed params). Other agent requests log `unsupported agent request` (`conversationId`, `method`) and get `-32601`. The tool title and option names are never logged; every agent string is cut to 64 runes with control and format runes replaced.

## T3 — `internal/api` (REST + SSE handlers, guard)

Implements: M3 · Decisions: `ADR-005`, `ADR-006`, `ADR-007`

Serves `/healthz` (`200 {"service":"agyd"}`), `/v1/status`, `/v1/current` (GET, PATCH), `/v1/prompt`, `/v1/cancel`, `/v1/clear` (POST -> `201`), `/v1/conversations` (POST, GET), `/v1/conversations/{conversationId}` (GET, PATCH, DELETE) with `/prompt`, `/cancel`, `/use`, `/clear` (POST -> `201`), `/v1/models`, and `/v1/defaults` (GET, PUT; PUT persists to `state.json`). Handlers call the `conversation.Manager`; prompts and cancels go to `acp.Service`.

- **Interfaces:** `NewHandler(log, svc acp.Service, mgr *conversation.Manager, cfg Config) http.Handler` (`Config.ExtraRoutes` mounts non-API routes such as `web.Routes()` on the same mux and method table so wrong methods return JSON `405 METHOD_NOT_ALLOWED` with `Allow`); `Routes() []Route` (checked against `openapi.json`, excluding `ExtraRoutes`); `Guard(next, extraHosts...)` (Host allowlist, `403 FORBIDDEN_HOST`, `http.CrossOriginProtection`).
- **Structured access logging and prompt lifecycle (`internal/api/log.go`, `prompt.go`):** outer `accessLogMiddleware` wraps `recoverMiddleware(mux)`. It records request duration and uses `statusWriter` (captures status code, defaults to 200, implements `Unwrap() http.ResponseWriter` preserving `http.ResponseController` flushing and deadlines for SSE) to emit `"http request"` access logs (`method`, `path`, `status`, `durationMs`, optional `conversationId`, `canceled`). `isQuietRoute(path)` demotes `2xx`/`3xx` responses on `/healthz`, `/`, `/docs`, `/openapi.json`, and `/static/*` to `DEBUG`. `prompt` emits `"prompt started"` (`conversationId`, `model`, `thinkingLevel`, `mode`, `stream`), and `logPromptFinished` emits `"prompt finished"` (`conversationId`, `stream`, `stopReason`, `durationMs`, `updates`, and optional `totalTokens` parsed via `usageTotalTokens`). Strict privacy: prompt, reply, tool arguments, query strings, and update bodies are never logged at any level.
- **Prompt envelope (`ADR-007`):** `POST /v1/prompt` and `POST /v1/conversations/{conversationId}/prompt` strictly wrap ACP v1 verbatim without inventing a custom `output.items` taxonomy or stripping/rewriting `agy` text (`text` is the verbatim concatenation of `agent_message_chunk` text chunks). Buffered JSON returns `promptResponse`, always serializing `updates` as a JSON array (`[]` when empty, never `null` and never omitted) alongside `conversationId`, `model`, `thinkingLevel`, `mode`, `stopReason`, `text`, and `usage`. SSE (`Accept: text/event-stream`) streams verbatim `session_update` events in arrival order and ends with `prompt_done` encoded via `promptDoneResponse` (omitting `updates`), or `prompt_error` on failure.
- **Headers:** `Agyd-Conversation`, `Agyd-Model`, `Agyd-Thinking-Level`, `Agyd-Mode` on every `/healthz` and `/v1/*` response (empty values omitted).
- **Errors:** AIP-193 envelope with `ErrorInfo.reason` in domain `agyd.local`. New reasons: `CONVERSATION_NOT_FOUND`, `NO_CURRENT_CONVERSATION`, `INVALID_SETTING` (format, unknown to agy, or agy `-32602`), `MODELS_UNKNOWN`. Existing: `PROMPT_IN_FLIGHT`, `INVALID_CWD` (also a `cwd` that is not an existing real directory, contains a symlink — naming the real path — or has any `.gemini` segment in any letter case), `MALFORMED_BODY` (also trailing data after the value), `ACP_*`, `UNAVAILABLE`, `FORBIDDEN_HOST`. Surface hardening: `METHOD_NOT_ALLOWED` (405 + `Allow`, from the route table), `BODY_TOO_LARGE` (413), `CROSS_ORIGIN_REQUEST` (403, CSRF deny handler). A handler error caused by the client disconnecting writes nothing and logs at INFO (`client closed request`). Pinned by `internal/api/errors_test.go`.
- **Limits:** request bodies 1 MiB (`413 BODY_TOO_LARGE`); `PATCH`/`DELETE` during a turn are `409 PROMPT_IN_FLIGHT`.

## T4 — `internal/web` (Swagger UI and OpenAPI)

Implements: M4 · Decisions: `ADR-002`, `ADR-005`

Serves the vendored Swagger UI 5.35.0 and the hand-written OpenAPI 3.1 spec via `//go:embed`: `GET`/`HEAD /{$}` (302 to `/docs`), `/docs`, `/openapi.json`, `/static/*`. Exposes `Routes() []Route` (mounted into `api.NewHandler` via `Config.ExtraRoutes` so wrong methods answer `405` with `Allow: GET, HEAD`) and `Register(mux)`. Sets `Content-Security-Policy` (no inline scripts) and `X-Content-Type-Options: nosniff`.

## T5 — `internal/config` (state directory, validation, state store)

Implements: M5 · Decisions: `ADR-006`

Owns state directory resolution, setting value validation, and atomic persistence of the flat `state.json` snapshot.

- **Interfaces:** `StateDir(getenv)` resolves the directory holding `state.json`: `AGYD_STATE_DIR` (must be an absolute path when set), else `$XDG_CONFIG_HOME/agyd` (when absolute), else `~/.config/agyd`. `ValidSettingValue(v)` checks `^[A-Za-z0-9._-]{1,64}$`. `NewStateStore(dir)` returns a `*StateStore` backed by `<dir>/state.json`.
- **Schema:** flat `State` struct holding `CurrentConversationID`, `Defaults` (`Settings`), and `Conversations` (`map[string]ConversationRecord{cwd, model, thinkingLevel, mode}`). Legacy files with an `"instances"` key load as an empty `State`.
- **Reads:** non-blocking (`O_NONBLOCK`) as a regular file capped at `MaxStateFileSize` (4 MiB). Corrupt JSON, relative conversation `cwd`, or invalid setting values return an error.
- **Writes:** atomic write via temp file + sync + rename with mode `0600` (directory `0700`).

## T6 — `internal/conversation` (current pointer and settings)

Implements: M6 · Decisions: `ADR-005`, `ADR-006`

`Manager` is the one owner of the current pointer, running defaults, per-conversation remembered settings (`memo`: cwd and settings), and `state.json` saves.

- **Interfaces:** `New(Options)`, `Startup` (resume from state and re-apply settings, else fresh with lenient defaults), `Resolve(id)` (`""` = current, created when empty, resumed after an agy restart), `Current`, `Create` (strict settings), `Clear(id)` (`""` = current; starts a new conversation with the source's folder and leniently applied settings, makes it current, and keeps the source intact in history), `Use` (resumes a non-live conversation routed from `state.json` folder, falling back to `session/list` `cwd` or default workspace), `Update` (strict), `Delete`, `List` (live merged with agy history, preferring remembered cwd and including unlisted conversations from state), `View`, `Defaults`, `SetDefaults(s, persist)`, `Models`. Emits metadata-only INFO lifecycle logs on conversation changes: `"conversation created"`, `"conversation cleared"`, `"conversation switched"`, `"conversation updated"`, `"conversation deleted"`, and `"defaults updated"`.
- **Concurrency:** `opMu` serializes pointer and settings changes (create, clear, use, resume, update, delete); `saveMu` serializes full `state.json` snapshots and disk writes (lock order: `opMu -> saveMu -> mu`). Startup flag/env `Defaults` override saved `state.json` defaults for the running process without overwriting saved defaults on disk.
- **Errors:** `ErrNoCurrent`, `ErrInvalidSetting`, `ErrNoModels`, plus the `acp` errors. Deleting a current conversation agy no longer has still clears the pointer.

## T7 — `cmd/agyd` control-plane CLI

Implements: M7 · Decisions: `ADR-005`, `ADR-006`, `ADR-007`

`current`, `list`, `new`, `use`, `clear`, `model`, `models`, `defaults`, and `help` form a thin stdlib HTTP control-plane CLI backed 1:1 by `/v1/*`, with readable text/table output by default and `--json` for scripts. The removed `ask` subcommand is rejected as an unknown command (exit `2`), and the CLI includes no SSE client or stdin prompt handling.

- **Base URL:** `--host`/`--port` > `AGYD_URL` > `AGYD_HOST`/`AGYD_PORT` > `/healthz` probe on `8080` then `3000` (`cli_target.go`; 1 s per-probe timeout, requires `200` `application/json` `{"service":"agyd"}`; neither `config.env` nor `./.env` are read); `0.0.0.0`/`::` dial loopback.
- **Output:** readable text/table output on stdout by default, one stderr info line (`agyd: conversation=... model=... thinking=... mode=...`) from the `Agyd-*` headers, `(saved to state.json)` on `agyd defaults --model ...`, and `--json` to print the raw API response for scripts.
- **Errors:** unknown commands (including `ask`) and usage errors send nothing (exit `2`); a non-2xx prints the AIP-193 message (exit `1`); an unreachable server names the URL. The client never follows redirects.

## Data model

agyd owns no database. Conversations and their history live in agy (`~/.gemini`). agyd keeps a single file in its state directory:

- `state.json` — flat JSON holding `currentConversationId`, `defaults` (`model`, `thinkingLevel`, `mode`), and `conversations` (`map[string]ConversationRecord{cwd, model, thinkingLevel, mode}`). Rewritten atomically via temp file + sync + rename on pointer, settings, or default changes.

## Testing strategy

See `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Build, run and deploy

See the `DEPLOY-local` record (`fyr record get DEPLOY-local`).

## Trace

Every component in `SDD.md` maps to its technical design section and test suite.

| Component | Section | Verified by |
|---|---|---|
| M1 | T1 | `TestConfigFromEnvAndFlags`, `TestListenServeFallback`, `TestServeExplicitPortInUse`, `TestRunLogsFallbackAndNonLoopbackWarningsAtBind`, `TestWorkspaceRejectsSymlinks`, `TestDispatchServeVersusClient`, `TestPickDefaults`, `TestPickDefaultsWithoutOptions`, `TestWantPicker`, `TestFileIsTerminal`, `TestRestartPreservesStateAcrossSIGTERM`, `TestServerPerFolderIsolationAndStateRouting`, `TestServerClearReusesFolderProcAndPreservesSettings`, `TestParseLogLevel`, `TestLogLevelEnvAndFlagPrecedence`, `TestNewLoggerLevelFiltering`, `TestMuxIsGuarded`, `TestMuxUnknownPathJSON`, `TestMuxWebRoutesMethodNotAllowed`, `TestListenNetwork`, `TestExposedHosts`, `TestLocalURL`, `TestWildcardV4DoesNotListenOnV6` |
| M2 | T2 | `TestClientInitializeAndSessionLifecycle`, `TestClientPromptStreamingAndPermission`, `TestClientCancelAndCrashRecovery`, `TestSessionConfigOptions`, `TestListAndResumeAgentSessions`, `TestSessionRejectsSymlinkCwd`, `TestSetConfigAndCloseRefusedDuringTurn`, `TestParseConfigOptionsSkipsUnknownShapes`, `TestPermissionDecisionLogged`, `TestPermissionDecisionMalformedAndOversized`, `TestAgentRequestRejectsForeignSession`, `TestClipBoundsAndSanitizes`, `TestUnsupportedAgentRequestLogged`, `TestPerFolderProcessIsolation`, `TestRelativeBinaryNotHijackedByWorkspaceDir`, `TestConcurrentFolderProcessLifecycle`, `TestApplySettingsUpdateFromStream`, `TestProcessLifecycleLogs` |
| M3 | T3 | `TestStatusAndHealthEndpoints`, `TestCreateConversation`, `TestClearEndpoints`, `TestListAndGetConversations`, `TestPatchSettings`, `TestDeleteCurrentClearsPointer`, `TestUseSwitchesPromptTarget`, `TestPatchAndDeleteDuringTurn`, `TestPromptCurrentAutoCreates`, `TestPromptVerbatimACPEnvelope`, `TestPromptSSEAndErrors`, `TestCancelEndpoints`, `TestHeadersOnEveryResponse`, `TestDefaultsRoundTrip`, `TestDefaultsRejectInjection`, `TestModels`, `TestClassifyErrorMapping`, `TestErrorShapes`, `TestExtraRoutesMethodTable`, `TestCwdRejectsSymlinks`, `TestCrossOriginRefusalIsJSON`, `TestClientClosedRequestIsNotAServerError`, `TestAccessLogAndPromptLifecycle`, `TestGuard`, `TestGuardExtraHosts` |
| M4 | T4 | `TestWebRoutes`, `TestOpenAPIRoutesMatchAPI` |
| M5 | T5 | `TestStateDir`, `TestValidSettingValue`, `TestStateStoreFlatSchemaAndPermissions`, `TestStateStoreUpgradesLegacyInstancesFileAsEmpty`, `TestStateStoreCorruptAndValidationErrors`, `TestStateStoreRefusesFIFOWithoutBlocking`, `TestStateStoreConcurrentSaves`, `TestStateStoreValidationAndSymlinkHardening` |
| M6 | T6 | `TestStartupCreatesAndSavesState`, `TestStartupResumesFromStateAndReappliesSettings`, `TestStartupFallsBackToFresh`, `TestStartupLogsRejectedDefault`, `TestStartupNilStateAndSettingsOfSanitizes`, `TestResolveConcurrentEmptyPointerCreatesOne`, `TestResolveResumesCurrentAfterAgentRestart`, `TestResolveExplicitIDIsLiveOnly`, `TestClearCurrentPreservesFolderAndSettings`, `TestClearByIDLiveRememberedAndHistory`, `TestClearErrorsAndConcurrency`, `TestUseSwitchesAndResumes`, `TestResumeRoutesFromStateFolderAndFallback`, `TestCreateSettingsAreStrict`, `TestUpdateAndDelete`, `TestUpdateRereadsAfterLock`, `TestDeleteNonLiveCurrentClearsPointer`, `TestSetDefaultsAndCreateSerializeStateSnapshots`, `TestModelsFollowLiveCurrent`, `TestDefaultsAndModels`, `TestListMergesLiveAndHistory`, `TestConversationLifecycleLogs`, `TestDefaultsUpdatedLog`, `TestConcurrentUseAndPrompt` |
| M7 | T7 | `TestCLICommandsHitTheirEndpoints`, `TestClientBaseURL`, `TestCLIProbeDiscovery`, `TestClientHelp`, `TestCLIErrorsShowInfoLineAndEnvelope`, `TestCLIJSONPrintsTheRawResponse`, `TestCLIUnreachableServer`, `TestCLIUsageErrorsSendNothing`, `TestCLIDoesNotFollowRedirects`; live smoke 1-6 (`GUIDE-testing`) |
