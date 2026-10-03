# docker-watcher Go conversion — Tasks

Status: **approved for implementation**. Approval artifacts and detailed plan are in `.superpowers/sdd/docker-watcher-go-conversion/` and `.omp/plans/docker-watcher-go-conversion.md`.

## Implementation milestones

### 1. Go module and core contracts
- Use the committed Go module and preserve its test-first public API assumptions.
- Implement environment parsing, safe JSON logs, atomic state, legacy state reading, and unknown-key preservation.
- Keep deterministic boundaries for HTTP, process execution, clocks, state persistence, and logs.

### 2. GitHub polling and image lifecycle
- Implement authenticated pagination, ownership filtering, pushed_at short-circuit, Dockerfile/branch/SHA change detection.
- Implement shallow clone, actual checked-out SHA, `.git` removal, bounded Docker build and sequential latest/seven-character pushes.
- Persist Graphify target before tag pushes; a save failure blocks both pushes. Preserve secret isolation and bounded diagnostics.
- Continue processing other repositories after an individual failure and touch the heartbeat on every cycle.

### 3. Graphify and attached Docker stream
- Implement Docker API negotiation and bounded container/exec create/start/inspect requests.
- Implement persistent lifecycle ordering, generation snapshots/coalescing, retry/backoff, same-ID-only restart, 409 reconciliation, terminal persistence, and unresolved-ID blocking.
- Implement TCP/unix attached transport, bounded HTTP/body/frame parsing, cancellation and cleanup.
- Keep worker ownership/single-flight separate from persistent state; inspect recovered IDs without reattaching.

### 4. Clean Go cutover
- Replace the Python runtime image with pinned multi-stage Go/Alpine while retaining pinned Docker CLI, Git, CA roots, and Compose contracts.
- Update root requirements/design/tasks and developer setup docs for Go.
- Remove Python implementation, tests, pytest configuration, and dependencies with no shim.
- The orchestrator runs full offline Go tests, at least 90% coverage, vet/format/race checks where practical, Compose validation, and independent review after implementation lands.

## Offline acceptance

Behavior tests must cover the golden paths and consumer-visible failure/edge cases from requirements, including state-save suppression boundaries, build/target-save/push order, secret redaction, Graphify recovery/retry/inspection invariants, and fragmented/capped/cancelled attached streams. Never contact GitHub, remote Git, Docker, registries, production Graphify, or deployment environments during implementation or tests.

## Operator-owned cutover

Operators own image bootstrap, exclusive stop/verify/backup/start, live verification, and rollback. Keep the old image reference and state safe; inspect persisted exec IDs and never clear an uninspectable attempt without independent evidence and separately authorized state repair. Never run Python and Go writers concurrently.
