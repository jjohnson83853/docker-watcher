# docker-watcher Go conversion — Tasks

Status: **confirmed by user**. Execution requires recorded critic approval and open implementation gate.

## Phase 0 — Requirements/design/tasks gate
- [x] Record requirements, design, and task checkpoints confirmed separately by user.
- [x] Record planner/critic history and approved amendments.
- [x] Freeze compatibility baseline in the approved plan and requirements: environment defaults, JSON log fields/events, state structures, HTTP/subprocess bounds, Compose container/health/resources/log settings, Loki selector, and alert literal/datasource placeholder.
- [x] Resolve attached transport feasibility with local scripted peers only; record evidence and preserve HTTP/body/multiplex/cancellation semantics. No daemon or live endpoint accessed.
- [x] Critic reviews this saved task specification and associated requirements/design; apply exact amendments and record verdict. Final verdict: **ACCEPT**; see progress ledger.
- [x] Commit gate artifacts and open conversion implementation gate: SpecDD commit `09f5a1f5185adcaacb456c662234e3d92daa1ed8`; `.superpowers/sdd/GATE_OPEN` now records this approved offline-only gate.
- Exit: baseline and transport evidence are recorded, approvals/verdicts and amendments are recorded; no implementation dispatch before the gate is open.

## Phase 1 — Go module, configuration, state, and logging
- [x] Select and pin Go toolchain/module metadata and Go entrypoint.
- [x] Port environment parsing/defaults and JSON stdout events with safe error rendering.
- [x] Port state loading/saving with atomic replacement, directory sync before save/load consumers, legacy bare-string support, exact unknown-number preservation, and Graphify data round-trip.
- [x] Add behavior tests first; retain deterministic test seams for clock, state, transport, and subprocess boundaries as needed.
- Exit: config/log/state behavior passes offline fixtures without state-schema migration.

## Phase 2 — GitHub polling and image builds
- [x] Port authenticated paginated repository discovery, ownership filter, pushed_at short-circuit, root Dockerfile and branch/SHA checks. Cap each successful JSON response at 8 MiB and reject pages above 100 entries without capping total pagination.
- [x] Port shallow clone, authoritative `git rev-parse HEAD`, `.git` removal and Docker CLI build. Immediately after a successful image build, persist the Graphify target before refresh scheduling or either tag push; if that save fails, emit `graphify_refresh`/`persist_failure` with `not_persisted=true` and do not push. Test save failure and the post-build/pre-push crash boundary.
- [x] Push `latest` and short-SHA tags sequentially; retain timeouts, safe bounded diagnostics, and per-repository continuation. Git clone/revision errors preserve safe numeric exit/timeout classification; the PAT credential file is removed as soon as clone succeeds.
- [x] Port cycle sequencing and heartbeat behavior.
- Exit: deterministic offline tests cover pagination/HTTP failures, unchanged and changed repos, build/target-save/push success/failure/timeouts, token non-disclosure, and continued processing. No external service access.

## Phase 3 — Graphify lifecycle and attached stream
- [x] Port Docker Engine version negotiation and container/exec create/start/inspect operations.
- [x] Port durable transition ordering, one workspace exec, per-repo retry/backoff, coalescing, and unresolved-ID blocking. Failed state persistence must suppress subsequent create/start/inspect/follow-on action.
- [x] Port bounded attached HTTP framing and Docker multiplex parsing, TCP/unix transport, bounded DNS/setup, cancellation, cleanup, and redaction.
- [x] Worker alone owns attached start/capture; main cycle remains nonblocking and guards the attempt slot until worker exit. Persisted IDs are inspected each normal cycle independently of worker acknowledgment; recovered running/terminal IDs are inspected, never reattached. Run-context cancellation suppresses new work and safely persists an exec ID created concurrently with cancellation.
- [x] Test same-ID retry eligibility, HTTP 409 reconciliation, running/uninspectable states, persistence failures, crash windows, per-cycle inspect limit, guard/worker interleavings, recovery without reattachment, and stream fragmentation/caps/truncation/cancellation/cleanup.
- Exit: no replacement/replay for unresolved execs; all tests use local scripted peers only.

## Phase 4 — Clean runtime cutover and independent review
- [x] Replace Python image entrypoint with pinned multi-stage Go build; retain pinned Git/Docker CLI and CA roots; omit Python.
- [x] Preserve Compose contracts and update setup/development documentation. Offline-check alert literal `build failed`, existing Loki selector, and the alert file's non-provisioned status/datasource UID placeholder; do not provision Grafana.
- [x] Remove Python modules, dependencies, and obsolete tests; do not leave a shim.
- [x] Run full Go tests with at least 90% source coverage, `go vet`, formatting, race checks where practical, and offline Compose validation.
- [x] Obtain code-reviewer, Go reviewer, and security-reviewer reviews; resolve critical/high findings. Security's accepted LOW residuals are recorded in the progress ledger.
- Exit: all offline acceptance criteria pass, PR is ready for user review with compact test/coverage evidence, and source/docs are a clean Go cutover. Agents do not build/push images or deploy.

## Phase 5 — User-owned exclusive cutover and operational validation
- [ ] User/operator retains old image digest or local rollback tag, stops the old writer and verifies it is stopped, then inspects/backs up the quiescent current state before exclusively starting Go.
- [ ] Operator performs post-merge bootstrap and exclusive rollout. Inspect every persisted exec ID before Graphify work. An uninspectable ID preserves state and pauses Graphify processing; unrelated polling/build work may continue only if it cannot mutate that attempt's state or create conflicting Graphify work. Resume Graphify only after independent proof of completion/nonexecution and separately authorized state repair.
- [ ] Operator verifies one cycle's repo count and heartbeat/health. Verify build result/tags only if a build occurs; unchanged healthy cycles have no build outcome. Check actual Loki labels/stream and live Grafana rule, Loki datasource UID, and default route/contact point. If absent/misconfigured, notification acceptance is blocked; provisioning needs separate user authorization and an operator delivery test.
- [ ] Rollback stops Go, retains its current state by default, reconciles later transitions before any snapshot restoration, and starts only the pinned prior image. If old image reference or state safety is uncertain, pause for operator recovery. Never run both versions concurrently.
- Exit: user-owned operational acceptance; no agent production action.
