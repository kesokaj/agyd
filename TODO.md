# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

(no active plan; `PLAN-2026-11-19-1214-plan-server-structured-logging-2026-11-19` completed 2026-11-19)

## Next

- Permission positive control (operator): with agy `toolPermission: request-review`, rerun the deny_all probe and confirm `permission decision` lines and refusals.

## Backlog

- 2026-11-19 (security phase-4b #2) optional configurable cap on concurrent per-folder `agy acp` child processes (declined for minimal Phase 4b: single-user local loopback tool).
- 2026-11-18 (permission probe) agy reported `rm -rf rmme.txt` as run while the file stayed (its own deny rule); agyd cannot see that refusal. Consider surfacing tool_call status from `session/update` in the CLI.
- 2026-11-18 (security permission-log #2) rate-limit or dedupe `unsupported agent request` lines per method (rebutted for now: agy is the operator's local child).
- 2026-11-18 (security r1 #7) `GET /v1/current` resumes a non-live conversation; consider a side-effect-free read.
- 2026-11-18 (reviewer r1 #4) `Manager.Resolve` falls back to a fresh conversation only on `ErrSessionNotFound`.
- 2026-11-18 (reviewer r1 #5) openapi.json: declare 400/401 on `use`, `current` and the PATCH routes.
- 2026-11-18 (reviewer r1) `-32602` from agy is always labelled `INVALID_SETTING`.
- 2026-11-18 (reviewer surface #1) agent-side settings updates reach the acp registry but not the Manager memos in the state file; after a restart the old mode or model comes back.
- 2026-11-18 (reviewer r1 #8) fill `SessionInfo.Title` for live conversations.
