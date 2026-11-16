# TODO

<!-- Now is the active phase and mirrors its plan record's boxes, updated in the commit that ticks them; on a
     conflict the plan record wins. Next holds at most three phases, one line each. Backlog holds unscheduled
     ideas, one line each, dated, with their source. An action only the operator can take goes in the newest
     handoff's Next Steps. -->

## Now

Phase 1 — ACP stdio client, HTTP/SSE gateway, and Docker runtime (`PLAN-2026-11-16-1505-plan-agy-api-acp-http-sse-gateway-docker-image-2`): build `internal/acp`, `internal/api`, `cmd/agy-api`, `Dockerfile`, and `docker-compose.yml`.

- [x] Cold-reader check (`REVIEW-2026-11-16-1511-review-plan-agy-acp-api-cold-read-2026-11-16`)
- [x] Implement `go.mod` and `internal/acp` (`types.go`, `client.go`, `acp_test.go`) → T2
- [x] Implement `internal/api` (`server.go`, `api_test.go`) → T3
- [x] Implement `cmd/agy-api` (`main.go`, `main_test.go`) → T1
- [x] Write `Dockerfile` and `docker-compose.yml` and run Colima container smoke check → T1
- [x] Checkpoint

## Next

<!-- At most three phases, one line each, each with a plan record or a date. -->

## Backlog

- Add multi-backend profile selector (`agy` vs `claude-agent-acp`) per session request (added 2026-11-16, user request)
