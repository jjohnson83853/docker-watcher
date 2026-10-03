# docker-watcher Go conversion — Requirements

Status: **confirmed by user**. This is a runtime-language conversion of the existing service, not a product expansion.

## Goal and scope

Replace the Python runtime with Go while preserving the existing repository polling/image-build service, Graphify workspace refresher, external environment/state/log contracts, Compose deployment contract, and security/reliability behavior. Do not add Docker event watching, inbound endpoints, multiple replicas, new telemetry, registry authentication, or dual-run behavior.

## Requirements

1. Enumerate authenticated GitHub repositories for `GITHUB_USERNAME`, paginate at 100/page, filter owned repositories, log zero-repository/API failure outcomes, and check a root `Dockerfile` on the selected/default branch.
2. Skip Dockerfile and commit requests when persisted `pushed_at` is unchanged. Build only when the selected branch SHA differs from saved state.
3. Shallow clone the selected branch and use `git rev-parse HEAD` as authoritative SHA. After successful image build, persist that repository's Graphify refresh target immediately, before refresh scheduling or either tag push. If this durable save fails, do not push tags or proceed as though refresh intent was recorded; test the failure and post-build/pre-push crash boundary offline. Then push `:latest` and `:<first-seven-SHA>` using the pinned Docker CLI. Continue after an individual repository failure.
4. Preserve environment names/defaults: required `GITHUB_USERNAME`, `GITHUB_TOKEN`; empty `BRANCH` follows repository default; `REGISTRY_URL=registry.localdomain:5000`; `STATE_FILE=/data/state.json`; `POLL_INTERVAL_SECONDS=300`; `BUILD_TIMEOUT_SECONDS=1800`; `HEARTBEAT_FILE=/data/heartbeat`; `DOCKER_HOST`; `DOCKER_BUILDKIT=0`; `GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS=10`. Preserve current parse/fallback behavior.
5. Remove `.git` before build context. PAT must never appear in logs, image/context, or rendered errors. Every subprocess is bounded by the configured timeout. Build/push failure logs include only a bounded 4000-character output tail; clone/rev-parse errors do not render argv/output.
6. Preserve JSON stdout event/field meanings and build-failure literal `build failed`; raw paths, Graphify stream output, exception/response content, and credentials remain undisclosed.
7. Preserve state schema, legacy bare-string SHA acceptance, unknown keys, Graphify `target`, `completed`, `failures`, `retry_due` and top-level `graphify.attempt`. State writes remain atomic using same-directory temp + rename. Language conversion alone must not cause rebuild/replay or discard retry state.
8. Preserve Graphify command `graphify update /workspace` in `graphify-mcp`, environment and Docker API negotiation; one workspace operation at a time; persist intent before external side effects; inspect at most once per cycle; per-repository 60-second exponential retry capped at 1800 seconds; and unresolved-exec blocking. Retry start is allowed only for the same persisted ID after inspect says `Running=false, ExitCode=null` and its persisted fallback/known-failure deadline has elapsed; reconcile 409 by inspecting that ID. Never replace an uninspectable ID absent independently established completion/nonexecution and separately authorized state repair. A failed state persistence suppresses subsequent external action.
9. Preserve bounded attached Docker stream behavior: TCP/unix, HTTP status/headers/body (content-length and chunked), fragmented/multiple multiplexed frames, stdout/stderr distinction, 1 MiB frame cap, 64 KiB combined payload scan cap, bounded DNS/connect/header setup, cancellation and worker cleanup, fixed diagnostic classes, and no command-wide Graphify timeout. Worker alone owns attached start and capture; it cannot mutate persistent state or release the persistent attempt. Main poll loop stays nonblocking and retains an in-memory single-flight guard until worker exit. Inspect every saved ID in the normal cycle independently of worker acknowledgment; recovered running/terminal IDs are inspected, never reattached. Cancellation/cleanup is bounded.
10. Preserve sequential polling, per-cycle heartbeat even on failure, and Compose health threshold (poll interval + build timeout + 120 seconds) with 2200-second start period.
11. Preserve image/tag, exact `container_name: docker-watcher`, `.env`, `/data` named volume, `restart: unless-stopped`, CPU 2 / memory 1GiB, JSON log rotation, Watchtower exclusion, TCP Docker proxy endpoint `tcp://docker-lxc.localdomain:2375`, BuildKit disabled, and no raw socket mount or registry auth.
12. Retire all Python runtime modules, dependencies, and obsolete Python tests in the same cutover; no compatibility shim.
13. Provide deterministic offline tests; no test accesses GitHub, remote git, Docker daemon, registry, or production Graphify. Full Go source-line coverage MUST be at least 90%.

## Security and operational constraints

Agents do not perform live Docker operations, image build/push, deployment, registry writes, SSH, or alert provisioning. Deployment/bootstrap and post-merge operational verification remain user-owned. No simultaneous Python/Go writers against state, `:latest`, or Graphify.

## Acceptance

All contracts above have deterministic offline behavioral evidence; full Go test coverage is at least 90%; `go vet`, format/race checks where practical, Compose validation and independent Go/general/security reviews pass; docs describe Go development and operator-owned exclusive cutover/rollback; obsolete Python paths are removed.