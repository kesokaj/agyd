# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

`PLAN-folder-isolation` (`ADR-006`; Phases 0-1 complete). Active: **Phase 2: no symlinks** — reject symlinks anywhere in `-workspace` or requested conversation folders.

- [ ] A symlink anywhere in a requested folder, or in `-workspace`, -> 400 `INVALID_CWD` (a startup error for `-workspace`). The message names the real path.
- [ ] Tests first: a symlink at the end of the path, in the middle, and a real path that passes.
- [ ] Checkpoint: change record, commit and push, handoff.

Remaining phases: P3 port 8080 then 3000 - P4b per-folder `agy acp` process (`cmd.Dir = folder`) - P5 `/clear` (new conversation, old kept in history) - P6 405 on web routes.

## Next

- CLI output hardening: strip terminal control characters from server and agent text.
- Permission positive control (operator): with agy `toolPermission: request-review`, rerun the deny_all probe and confirm `permission decision` lines and refusals.

## Backlog

- 2026-11-18 (permission probe) agy reported `rm -rf rmme.txt` as run while the file stayed (its own deny rule); agyd cannot see that refusal. Consider surfacing tool_call status from `session/update` in the CLI.
- 2026-11-18 (security permission-log #2) rate-limit or dedupe `unsupported agent request` lines per method (rebutted for now: agy is the operator's local child).
- 2026-11-18 (security r1 #7) `GET /v1/current` resumes a non-live conversation; consider a side-effect-free read.
- 2026-11-18 (reviewer r1 #4) `Manager.Resolve` falls back to a fresh conversation only on `ErrSessionNotFound`.
- 2026-11-18 (reviewer r1 #5) openapi.json: declare 400/401 on `use`, `current` and the PATCH routes.
- 2026-11-18 (reviewer r1) `-32602` from agy is always labelled `INVALID_SETTING`.
- 2026-11-18 (reviewer surface #1) agent-side settings updates reach the acp registry but not the Manager memos in the state file; after a restart the old mode or model comes back.
- 2026-11-18 (reviewer r1 #8) fill `SessionInfo.Title` for live conversations.
