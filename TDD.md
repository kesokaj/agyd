# TDD — agyd

Technical design for `cmd/agyd` (serve and client), `internal/acp`, `internal/api`, `internal/web`, `internal/config` and `internal/conversation`, linking each component to its verification tests.
Status: agreed · Last updated: 2026-11-18 · Requirements: [PRD.md](PRD.md) · Design: [SDD.md](SDD.md)

## T1 — `cmd/agyd` serve (entry point, config, startup)

Implements: M1 · Spec: `SPEC-agy-api-acp-gateway` · Decisions: `ADR-004`, `ADR-005`

The first argument picks the mode: none, a flag, or `serve` starts the server; a known command name runs the client (T7). Serve resolves settings through `config.Load`, validates them, starts the ACP client, runs `Manager.Startup` (60 s), composes `internal/web` and `internal/api` under `api.Guard`, and shuts down on `SIGINT`/`SIGTERM`.

- **Interfaces:** flags `-host`, `-port`, `-workspace`, `-permission-policy`, `-agy`, `-acp-args`, `-model`, `-thinking`, `-mode`, `-no-prompt`, `-h`; each has an `AGYD_*` env twin. Precedence: flag > `./.env` (`AGYD_MODEL`, `AGYD_THINKING` only) > shell > `config.env`.
- **Startup picker** (`picker.go`, `startup.go`): runs only on a terminal, with agy ready, when no layer set `AGYD_MODEL` and `-no-prompt` is off. It offers agy's models and thinking levels and, on "save", writes them through `configEnvWriter` (only the keys given).
- **Errors:** exit `1` with a JSON `ERROR` line on invalid host/port/policy, unknown flags (the removed `-addr` included), positional arguments, a missing `agy` on `$PATH`, a malformed or non-regular env file, or an env file over 1 MiB. A non-loopback `-host` logs a `WARNING` naming the accepted hosts. Refused `./.env` keys are logged and ignored.

## T2 — `internal/acp` (ACP v1 JSON-RPC 2.0 stdio client)

Implements: M2 · Spec: `SPEC-agy-api-acp-gateway`

Manages the `agy acp` child over NDJSON-RPC 2.0. It multiplexes calls, fans `session/update` notifications out to subscribers, answers `session/request_permission` per policy, and respawns the child lazily after an exit.

- **Interfaces:** `NewClient`, `Status`, `NewSession`, `GetSession`, `ListSessions`, `CloseSession`, `Prompt`, `Cancel`, `SetConfigOption` (`session/set_config_option`), `ListAgentSessions` (`session/list`, agy's history), `ResumeSession` (`session/resume`; settings reset to agy defaults).
- **Errors:** `*RPCError`, `ErrProcessClosed`, `ErrSessionNotFound`, `ErrPromptInFlight` (also for set-config and close during a turn), `ErrInvalidArgument` (`-32602`, e.g. an unknown model), `ErrInvalidConfig`.
- **Permission policy:** `ALLOW_ALL` picks the first allow option, `DENY_ALL` the first reject option, else cancels. It applies only to requests agy sends (`ADR-005` known limit).

## T3 — `internal/api` (REST + SSE handlers, guard)

Implements: M3 · Spec: `SPEC-agy-api-acp-gateway` · Decision: `ADR-005`

Serves `/healthz`, `/v1/status`, `/v1/current` (GET, PATCH), `/v1/prompt`, `/v1/cancel`, `/v1/conversations` (POST, GET), `/v1/conversations/{conversationId}` (GET, PATCH, DELETE) with `/prompt`, `/cancel`, `/use`, `/v1/models`, and `/v1/defaults` (GET, PUT). Handlers call the `conversation.Manager`; prompts and cancels go to `acp.Service`.

- **Interfaces:** `NewHandler(log, svc acp.Service, mgr *conversation.Manager, cfg Config) http.Handler`; `Routes() []Route` (checked against `openapi.json`); `Guard(next, extraHosts...)` (Host allowlist, `403 FORBIDDEN_HOST`, `http.CrossOriginProtection`).
- **Headers:** `Agyd-Conversation`, `Agyd-Model`, `Agyd-Thinking-Level`, `Agyd-Mode` on every `/healthz` and `/v1/*` response (empty values omitted).
- **Errors:** AIP-193 envelope with `ErrorInfo.reason` in domain `agyd.local`. New reasons: `CONVERSATION_NOT_FOUND`, `NO_CURRENT_CONVERSATION`, `INVALID_SETTING` (format, unknown to agy, or agy `-32602`), `MODELS_UNKNOWN`. Existing: `PROMPT_IN_FLIGHT`, `INVALID_CWD`, `MALFORMED_BODY`, `ACP_*`, `UNAVAILABLE`, `FORBIDDEN_HOST`.
- **Limits:** request bodies 1 MiB; `PATCH`/`DELETE` during a turn are `409 PROMPT_IN_FLIGHT`.

## T4 — `internal/web` (Swagger UI and OpenAPI)

Implements: M4 · Spec: `SPEC-agy-api-acp-gateway`

Serves the vendored Swagger UI 5.35.0 and the hand-written OpenAPI 3.1 spec via `//go:embed`: `GET /{$}` (302 to `/docs`), `/docs`, `/openapi.json`, `/static/*`. Sets `Content-Security-Policy` (no inline scripts) and `X-Content-Type-Options: nosniff`.

## T5 — `internal/config` (env files, layering, state)

Implements: M5 · Decisions: `ADR-005` (D9, D24)

- **Interfaces:** `ParseEnv(r, name)` reads only `AGYD_*` lines (quotes, comments, `export`, later duplicates win; malformed AGYD lines fail with `name:line`; lines up to 1 MiB). `ReadEnvFile(path)` follows symlinks, refuses non-regular files and files over `MaxEnvFileSize` (1 MiB). `Load(cwd, getenv, read)` returns `*Layered` (`Getenv`, `Source`, `Ignored`, `ConfigFile`, `DotEnvFile`) and never touches the process environment. `FindDotEnv` walks up from cwd, stopping at `$HOME` inside it. `Dir` is `$XDG_CONFIG_HOME/agyd`, else `~/.config/agyd`.
- **Allowlists:** `DotEnvKeys` (`AGYD_MODEL`, `AGYD_THINKING`) is what `./.env` may set; `SettingKeys` (plus `AGYD_MODE`) is what `WriteConfigEnv` may write.
- **Writes:** `WriteConfigEnv(path, updates)` validates keys and values (`ValidSettingValue`: `^[A-Za-z0-9._-]{1,64}$`) before touching the file, refuses to merge into a malformed file, keeps other lines verbatim, and writes atomically (0600 temp file, rename, 0700 dir). `NewStateStore(dir, port)` keeps `state.json` keyed by port with the same atomic write; a corrupt file reads as empty.

## T6 — `internal/conversation` (current pointer and settings)

Implements: M6 · Decision: `ADR-005`

`Manager` is the one owner of the current pointer, running defaults, per-conversation remembered settings (`memo`: cwd and settings) and `state.json` saves.

- **Interfaces:** `New(Options)`, `Startup` (resume from state and re-apply settings, else fresh with lenient defaults), `Resolve(id)` (`""` = current, created when empty, resumed after an agy restart), `Current`, `Create` (strict settings), `Use` (resumes a non-live conversation using its memo or history cwd), `Update` (strict), `Delete`, `List` (live merged with agy history), `View`, `Defaults`, `SetDefaults(s, persist)`, `Models`.
- **Concurrency:** `opMu` serializes pointer and settings changes; `Update` re-reads the conversation after taking it. `defaultsMu` serializes `SetDefaults`, so `config.env` merges lose no key. `mu` guards the fields.
- **Errors:** `ErrNoCurrent`, `ErrInvalidSetting`, `ErrNoModels`, plus the `acp` errors. Deleting a current conversation agy no longer has still clears the pointer.

## T7 — `cmd/agyd` client (`agyd ask` and friends)

Implements: M7 · Decision: `ADR-005`

`ask`, `new`, `list`, `use`, `current`, `model`, `models`, `defaults`, `help`: a stdlib HTTP client of a running agyd.

- **Base URL:** `--host`/`--port` > shell `AGYD_HOST`/`AGYD_PORT` > `config.env` (never `./.env`); `0.0.0.0`/`::` dial loopback.
- **Output:** one stderr info line (`agyd: conversation=... model=... thinking=... mode=...`) from the `Agyd-*` headers; `ask` streams SSE text to stdout and reads the prompt from stdin when none is given; `--json` prints the raw response.
- **Errors:** usage errors send nothing (exit 2); a non-2xx prints the AIP-193 message (exit 1); an unreachable server names the URL. The client never follows redirects.

## Data model

agyd owns no database. Conversations and their history live in agy (`~/.gemini`). agyd keeps two files in its config dir:

- `config.env` — operator settings; agyd writes only `AGYD_MODEL`, `AGYD_THINKING`, `AGYD_MODE`.
- `state.json` — `{"<port>": {currentConversationId, cwd, model, thinkingLevel, mode}}`, rewritten on every pointer or settings change.

## Testing strategy

See `GUIDE-testing` (`fyr record get GUIDE-testing`).

## Build, run and deploy

See the `DEPLOY-local` record (`fyr record get DEPLOY-local`).

## Trace

Every component in `SDD.md` maps to its technical design section and test suite.

| Component | Section | Verified by |
|---|---|---|
| M1 | T1 | `TestConfigFromEnvAndFlags`, `TestDispatchServeVersusClient`, `TestConfigEnvWriterSavesOnlyGivenKeys`, `TestPickDefaults`, `TestPickDefaultsWithoutOptions`, `TestWantPicker`, `TestMuxIsGuarded`, `TestMuxUnknownPathJSON`, `TestListenNetwork`, `TestExposedHosts`, `TestLocalURL`, `TestWildcardV4DoesNotListenOnV6` |
| M2 | T2 | `TestClientInitializeAndSessionLifecycle`, `TestClientPromptStreamingAndPermission`, `TestClientCancelAndCrashRecovery`, `TestSessionConfigOptions`, `TestListAndResumeAgentSessions`, `TestSetConfigAndCloseRefusedDuringTurn`, `TestParseConfigOptionsSkipsUnknownShapes` |
| M3 | T3 | `TestStatusAndHealthEndpoints`, `TestCreateConversation`, `TestListAndGetConversations`, `TestPatchSettings`, `TestDeleteCurrentClearsPointer`, `TestUseSwitchesPromptTarget`, `TestPatchAndDeleteDuringTurn`, `TestPromptCurrentAutoCreates`, `TestPromptSSEAndErrors`, `TestCancelEndpoints`, `TestHeadersOnEveryResponse`, `TestDefaultsRoundTrip`, `TestDefaultsRejectInjection`, `TestModels`, `TestClassifyErrorMapping`, `TestGuard`, `TestGuardExtraHosts` |
| M4 | T4 | `TestWebRoutes`, `TestOpenAPIRoutesMatchAPI` |
| M5 | T5 | `TestParseEnv`, `TestReadEnvFile`, `TestOpenEnvFileNeverBlocks`, `TestValidSettingValue`, `TestDir`, `TestFindDotEnv`, `TestLoadPrecedence`, `TestLoadMissingFilesAndErrors`, `TestLoadNeverTouchesProcessEnv`, `TestWriteConfigEnv`, `TestStateStore`, `TestStateStoreConcurrentSaves`, `TestStateStoreCorruptFile` |
| M6 | T6 | `TestStartupCreatesAndSavesState`, `TestStartupResumesFromStateAndReappliesSettings`, `TestStartupFallsBackToFresh`, `TestStartupLogsRejectedDefault`, `TestResolveConcurrentEmptyPointerCreatesOne`, `TestResolveResumesCurrentAfterAgentRestart`, `TestResolveExplicitIDIsLiveOnly`, `TestUseSwitchesAndResumes`, `TestCreateSettingsAreStrict`, `TestUpdateAndDelete`, `TestUpdateRereadsAfterLock`, `TestDeleteNonLiveCurrentClearsPointer`, `TestSetDefaultsSerializesPersist`, `TestDefaultsAndModels`, `TestListMergesLiveAndHistory`, `TestConcurrentUseAndPrompt` |
| M7 | T7 | `TestCLIAskStreaming`, `TestCLICommandsHitTheirEndpoints`, `TestClientBaseURL`, `TestClientHelp`, `TestCLIErrorsShowInfoLineAndEnvelope`, `TestCLIJSONPrintsTheRawResponse`, `TestCLIUnreachableServer`, `TestCLIUsageErrorsSendNothing`, `TestCLIDoesNotFollowRedirects`, `TestServeAndCLIReadEnvSafely`; live smoke 1-6 (`GUIDE-testing`) |
