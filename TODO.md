# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

Phase 2 — `agyd` host binary with Swagger docs, Docker removed (`PLAN-2026-11-16-1821-plan-agyd-host-binary-with-swagger-docs-and-web`, `ADR-002`).

- [x] P1: `cmd/agyd`, loopback-only config, `api.Guard`, `api.Routes`, status `workspace`, Docker removal, tests
- [x] P2: `internal/web` (`openapi.json`, vendored Swagger UI 5.35.0, `/` redirect), tests
- [x] P3: docs and records, reviewer + security + verifier, commit

## Next

- Rebuild with Go 1.27.4 (govulncheck GO-2026-6859, net/http crash in 1.27.3)
- Add `claude-agent-acp` as a second backend behind `-agy`/`-acp-args` (ACP seam already in place)

## Backlog

- Add multi-backend profile selector (`agy` vs `claude-agent-acp`) per session request (added 2026-11-16, user request)
