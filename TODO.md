# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

Phase 3 — HTTP API (`PLAN-model-selection`, D2/D5/D7, D10-D21): freeze `openapi.json`, `/v1/conversations` + `/v1/current` + `/v1/defaults` + `/v1/models`, `Agyd-*` headers, handlers on `conversation.Manager`.

- [x] Phase 0: ACP method check (live probe) and cold-read fold-in
- [x] Phase 1: config layering and defaults
- [x] Phase 2: ACP plus current conversation
- [ ] Phase 3: HTTP API

## Next

- Phase 4 — CLI client (`agyd ask|new|list|use|current|model|models|defaults`, `--json`).
- Phase 5 — docs, ADR-005, GUIDE-testing/DEPLOY-local updates, reviewer + security, live smoke.

## Backlog
