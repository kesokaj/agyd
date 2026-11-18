# TDD — agyd

Technical design for `cmd/agyd` (serve and client), `internal/acp`, `internal/api`, `internal/web`, `internal/config` and `internal/conversation`, linking each component to its verification tests.
Status: agreed · Last updated: 2026-11-18 · Requirements: [PRD.md](PRD.md) · Design: [SDD.md](SDD.md)

## T1 — `cmd/agyd` serve (entry point, config, startup)

Implements: M1 · Spec: `SPEC-agy-api-acp-gateway` · Decisions: `ADR-004`, `ADR-005`, `ADR-006`

The first argument picks the mode: none, a flag, or `serve` starts the server; a known command name runs the client (T7). Serve resolves settings through flags and `AGYD_*` env vars, falling back to saved defaults in `state.json` (for model, thinking, mode), then built-in defaults. It validates settings, starts the ACP client, runs `Manager.Startup` (60 s), composes `internal/web` and `internal/api` under `api.Guard`, and shuts down on `SIGINT`/`SIGTERM`.

- **Interfaces:** flags `-host`, `-port`, `-workspace`, `-state-dir`, `-permission-policy`, `-agy`, `-acp-args`, `-model`, `-thinking`, `-mode`, `-no-prompt`, `-h`; each has an `AGYD_*` env twin. Precedence: flags > `AGYD_*` env vars > saved defaults in `state.json` > built-in defaults.
- **Startup picker** (`picker.go`, `startup.go`, `tty_*.go`): runs only on a real terminal (`TIOCGETA`/`TIOCGWINSZ` ioctl, never `/dev/null`), with agy ready, when no flag, env var, or saved default set a model and `-no-prompt` is off. It offers agy's models and thinking levels and, on "save", writes them to `state.json` defaults via Manager.
- **Errors:** exit `1` with a JSON `ERROR` line on invalid host/port/policy/state-dir, unknown flags, positional arguments, or a missing `agy` on `$PATH`. A non-loopback `-host` logs a `WARNING` naming the accepted hosts.

## T2 — `internal/acp` (ACP v1 JSON-RPC 2.0 stdio client)

Implements: M2 · Spec: `SPEC-agy-api-acp-gateway`

Manages the `agy acp` child over NDJSON-RPC 2.0. It multiplexes calls, fans `session/update` notifications out to subscribers, answers `session/request_permission` per policy, and respawns the child lazily after an exit.

- **Interfaces:** `NewClient`, `Status`, `NewSession`, `GetSession`, `ListSessions`, `CloseSession`, `Prompt`, `Cancel`, `SetConfigOption` (`session/set_config_option`), `ListAgentSessions` (`session/list`, agy's history), `ResumeSession` (`session/resume`; settings reset to agy defaults).
- **Errors:** `*RPCError`, `ErrProcessClosed`, `ErrSessionNotFound`, `ErrPromptInFlight` (also for set-config and close during a turn), `ErrInvalidArgument` (`-32602`, e.g. an unknown model), `ErrInvalidConfig`.
- **Permission policy:** `ALLOW_ALL` picks the first allow option, `DENY_ALL` the first reject option, else cancels. It applies only to requests agy sends (`ADR-005` known limit); agy sends none while its own `toolPermission` is `always-proceed`.
- **Permission log:** every `session/request_permission` answer writes one INFO line `permission decision` (`conversationId`, `toolCallId`, `toolKind`, the first 8 `options` as `optionId`/`kind`, `optionCount`, `policy`, `outcome`, `optionId`, `optionKind`, `err` on malformed params). Other agent requests log `unsupported agent request` (`conversationId`, `method`) and get `-32601`. The tool title and option names are never logged; every agent string is cut to 64 runes with control and format runes replaced.

## T3 — `internal/api` (REST + SSE handlers, guard)

Implements: M3 · Spec: `SPEC-agy-api-acp-gateway` · Decisions: `ADR-005`, `ADR-006`

Serves `/healthz`, `/v1/status`, `/v1/current` (GET, PATCH), `/v1/prompt`, `/v1/cancel`, `/v1/conversations` (POST, GET), `/v1/conversations/{conversationId}` (GET, PATCH, DELETE) with `/prompt`, `/cancel`, `/use`, `/v1/models`, and `/v1/defaults` (GET, PUT; PUT persists to `state.json`). Handlers call the `conversation.Manager`; prompts and cancels go to `acp.Service`.

- **Interfaces:** `NewHandler(log, svc acp.Service, mgr *conversation.Manager, cfg Config) http.Handler`; `Routes() []Route` (checked against `openapi.json`); `Guard(next, extraHosts...)` (Host allowlist, `403 FORBIDDEN_HOST`, `http.CrossOriginProtection`).
- **Headers:** `Agyd-Conversation`, `Agyd-Model`, `Agyd-Thinking-Level`, `Agyd-Mode` on every `/healthz` and `/v1/*` response (empty values omitted).
- **Errors:** AIP-193 envelope with `ErrorInfo.reason` in domain `agyd.local`. New reasons: `CONVERSATION_NOT_FOUND`, `NO_CURRENT_CONVERSATION`, `INVALID_SETTING` (format, unknown to agy, or agy `-32602`), `MODELS_UNKNOWN`. Existing: `PROMPT_IN_FLIGHT`, `INVALID_CWD` (also a `cwd` that is not an existing directory, or whose symlink-resolved path or any letter case of a segment is `.gemini`), `MALFORMED_BODY` (also trailing data after the value), `ACP_*`, `UNAVAILABLE`, `FORBIDDEN_HOST`. Surface hardening: `METHOD_NOT_ALLOWED` (405 + `Allow`, from the route table), `BODY_TOO_LARGE` (413), `CROSS_ORIGIN_REQUEST` (403, CSRF deny handler). A handler error caused by the client disconnecting writes nothing and logs at INFO (`client closed request`). Pinned by `internal/api/errors_test.go`.
- **Limits:** request bodies 1 MiB (`413 BODY_TOO_LARGE`); `PATCH`/`DELETE` during a turn are `409 PROMPT_IN_FLIGHT`.

## T4 — `internal/web` (Swagger UI and OpenAPI)

Implements: M4 · Spec: `SPEC-agy-api-acp-gateway`

Serves the vendored Swagger UI 5.35.0 and the hand-written OpenAPI 3.1 spec via `//go:embed`: `GET /{$}` (302 to `/docs`), `/docs`, `/openapi.json`, `/static/*`. Sets `Content-Security-Policy` (no inline scripts) and `X-Content-Type-Options: nosniff`.

## T5 — `internal/config` (state directory, validation, state store)

Implements: M5 · Decisions: `ADR-006`

- **Interfaces:** `StateDir(getenv)` resolves the directory holding `state.json`: `AGYD_STATE_DIR` (must be an absolute path when set), else `$XDG_CONFIG_HOME/agyd` (when absolute), else `~/.config/agyd`. `ValidSettingValue(v)` checks `^[A-Za-z0-9._-]{1,64}$`. `NewStateStore(dir)` returns a `*StateStore` backed by `<dir>/state.json`.
- **Schema:** flat `State` struct holding `CurrentConversationID`, `Defaults` (`Settings`), and `Conversations` (`map[string]ConversationRecord{cwd, model, thinkingLevel, mode}`). Legacy files with an `"instances"` key load as an empty `State`.
- **Reads:** non-blocking (`O_NONBLOCK`) as a regular file capped at `MaxStateFileSize` (4 MiB). Corrupt JSON, relative conversation `cwd`, or invalid setting values return an error.
- **Writes:** atomic write via temp file + sync + rename with mode `0600` (directory `0700`).

## T6 — `internal/conversation` (current pointer and settings)

Implements: M6 · Decisions: `ADR-005`, `ADR-006`

`Manager` is the one owner of the current pointer, running defaults, per-conversation remembered settings (`memo`: cwd and settings), and `state.json` saves.

- **Interfaces:** `New(Options)`, `Startup` (resume from state and re-apply settings, else fresh with lenient defaults), `Resolve(id)` (`""` = current, created when empty, resumed after an agy restart), `Current`, `Create` (strict settings), `Use` (resumes a non-live conversation using its memo or history cwd), `Update` (strict), `Delete`, `List` (live merged with agy history, preferring remembered cwd and including unlisted conversations from state), `View`, `Defaults`, `SetDefaults(s, persist)`, `Models`.
- **Concurrency:** `opMu` serializes pointer and settings changes (create, use, resume, update, delete); `saveMu` serializes full `state.json` snapshots and disk writes (lock order: `opMu -> saveMu -> mu`). Startup flag/env `Defaults` override saved `state.json` defaults for the running process without overwriting saved defaults on disk.
- **Errors:** `ErrNoCurrent`, `ErrInvalidSetting`, `ErrNoModels`, plus the `acp` errors. Deleting a current conversation agy no longer has still clears the pointer.

## T7 — `cmd/agyd` client (`agyd ask` and friends)

Implements: M7 · Decisions: `ADR-005`, `ADR-006`

`ask`, `new`, `list`, `use`, `current`, `model`, `models`, `defaults`, `help`: a stdlib HTTP client of a running agyd.

- **Base URL:** `--host`/`--port` > shell `AGYD_HOST`/`AGYD_PORT` > `127.0.0.1:8080` (neither `config.env` nor `./.env` are read); `0.0.0.0`/`::` dial loopback.
- **Output:** one stderr info line (`agyd: conversation=... model=... thinking=... mode=...`) from the `Agyd-*` headers; `ask` streams SSE text to stdout and reads the prompt from stdin when none is given; `agyd defaults --model ...` prints `(saved to state.json)`; `--json` prints the raw response.
- **Errors:** usage errors send nothing (exit 2); a non-2xx prints the AIP-193 message (exit 1); an unreachable server names the URL. The client never follows redirects.

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
| M1 | T1 | `TestConfigFromEnvAndFlags`, `TestDispatchServeVersusClient`, `TestPickDefaults`, `TestPickDefaultsWithoutOptions`, `TestWantPicker`, `TestFileIsTerminal`, `TestRestartPreservesStateAcrossSIGTERM`, `TestMuxIsGuarded`, `TestMuxUnknownPathJSON`, `TestListenNetwork`, `TestExposedHosts`, `TestLocalURL`, `TestWildcardV4DoesNotListenOnV6` |
| M2 | T2 | `TestClientInitializeAndSessionLifecycle`, `TestClientPromptStreamingAndPermission`, `TestClientCancelAndCrashRecovery`, `TestSessionConfigOptions`, `TestListAndResumeAgentSessions`, `TestSetConfigAndCloseRefusedDuringTurn`, `TestParseConfigOptionsSkipsUnknownShapes`, `TestPermissionDecisionLogged`, `TestPermissionDecisionMalformedAndOversized`, `TestClipBoundsAndSanitizes`, `TestUnsupportedAgentRequestLogged` |
| M3 | T3 | `TestStatusAndHealthEndpoints`, `TestCreateConversation`, `TestListAndGetConversations`, `TestPatchSettings`, `TestDeleteCurrentClearsPointer`, `TestUseSwitchesPromptTarget`, `TestPatchAndDeleteDuringTurn`, `TestPromptCurrentAutoCreates`, `TestPromptSSEAndErrors`, `TestCancelEndpoints`, `TestHeadersOnEveryResponse`, `TestDefaultsRoundTrip`, `TestDefaultsRejectInjection`, `TestModels`, `TestClassifyErrorMapping`, `TestGuard`, `TestGuardExtraHosts` |
| M4 | T4 | `TestWebRoutes`, `TestOpenAPIRoutesMatchAPI` |
| M5 | T5 | `TestStateDir`, `TestValidSettingValue`, `TestStateStoreFlatSchemaAndPermissions`, `TestStateStoreUpgradesLegacyInstancesFileAsEmpty`, `TestStateStoreCorruptAndValidationErrors`, `TestStateStoreRefusesFIFOWithoutBlocking`, `TestStateStoreConcurrentSaves`, `TestStateStoreValidationAndSymlinkHardening` |
| M6 | T6 | `TestStartupCreatesAndSavesState`, `TestStartupResumesFromStateAndReappliesSettings`, `TestStartupFallsBackToFresh`, `TestStartupLogsRejectedDefault`, `TestStartupNilStateAndSettingsOfSanitizes`, `TestResolveConcurrentEmptyPointerCreatesOne`, `TestResolveResumesCurrentAfterAgentRestart`, `TestResolveExplicitIDIsLiveOnly`, `TestUseSwitchesAndResumes`, `TestCreateSettingsAreStrict`, `TestUpdateAndDelete`, `TestUpdateRereadsAfterLock`, `TestDeleteNonLiveCurrentClearsPointer`, `TestSetDefaultsAndCreateSerializeStateSnapshots`, `TestModelsFollowLiveCurrent`, `TestDefaultsAndModels`, `TestListMergesLiveAndHistory`, `TestConcurrentUseAndPrompt` |
| M7 | T7 | `TestCLIAskStreaming`, `TestCLICommandsHitTheirEndpoints`, `TestClientBaseURL`, `TestClientHelp`, `TestCLIErrorsShowInfoLineAndEnvelope`, `TestCLIJSONPrintsTheRawResponse`, `TestCLIUnreachableServer`, `TestCLIUsageErrorsSendNothing`, `TestCLIDoesNotFollowRedirects`; live smoke 1-6 (`GUIDE-testing`) |
