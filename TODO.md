# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

No active phase. `PLAN-model-selection` is complete (Phase 5 closed 2026-11-18, ADR-005).

## Next

- Persist the creation cwd of agyd-created conversations (agy `session/list` reports the git root).
- CLI output hardening: strip terminal control characters from server and agent text.
- Permission positive control (operator): with agy `toolPermission: request-review`, rerun the deny_all probe and confirm `permission decision` lines and refusals.

## Backlog

- 2026-11-18 (permission probe) agy reported `rm -rf rmme.txt` as run while the file stayed (its own deny rule); agyd cannot see that refusal. Consider surfacing tool_call status from `session/update` in the CLI.
- 2026-11-18 (security permission-log #2) rate-limit or dedupe `unsupported agent request` lines per method (rebutted for now: agy is the operator's local child).
- 2026-11-18 (security r1 #7) `GET /v1/current` resumes a non-live conversation; consider a side-effect-free read.
- 2026-11-18 (reviewer r1 #4) `Manager.Resolve` falls back to a fresh conversation only on `ErrSessionNotFound`.
- 2026-11-18 (reviewer r1 #5) openapi.json: declare 400/401 on `use`, `current` and the PATCH routes.
- 2026-11-18 (reviewer r1) `-32602` from agy is always labelled `INVALID_SETTING`.
- 2026-11-18 (reviewer r1 #7) store agy `config_option_update`/`current_mode_update` notifications.
- 2026-11-18 (reviewer r1 #8) fill `SessionInfo.Title` for live conversations.
- 2026-11-18 (security r2 #2) skip malformed lines for ignored keys in `./.env` instead of failing startup; lines over 1 MiB still fail.
- 2026-11-18 (security r2 #3) warn when a refused `./.env` is skipped; owner check on the walk outside `$HOME` (it goes up to `/`).
- 2026-11-18 (reviewer r2 #1) cross-process lock (flock) on `config.env` writes.
