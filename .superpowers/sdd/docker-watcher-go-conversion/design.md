# docker-watcher Go conversion — Design

Status: **confirmed by user**. Design is constrained to behavior-preserving implementation.

## Runtime architecture

- Implement the service in Go with responsibility boundaries matching `app.py`, `graphify_refresh.py`, and `graphify_stream.py`: polling/build orchestration, durable state, Graphify lifecycle, and bounded attached-stream parsing.
- Use Go standard-library HTTP for GitHub and the narrowly scoped Docker Engine API unless offline evidence demonstrates a dependency is necessary and fully preserves attached-stream behavior. Prefer explicit narrow transport over a general Docker SDK.
- Retain `git` and pinned `docker-cli=29.5.3-r1` subprocess build/push path. Use context-bounded processes, protect argv/output from logs, and strip `.git` before Docker build context.
- Preserve current JSON state representation and unknown fields, legacy SHA strings, atomic same-directory replacement, and Graphify transitions. Inject clock/state/transport/process boundaries only where required for deterministic behavioral tests.
- After a successful image build, persist its per-repository Graphify target immediately, before scheduling refresh or pushing either tag. A failed persistence blocks tag push and all later external side effects; tests prove save failure and post-build/pre-push interruption.
- Preserve Graphify one-operation lifecycle, persistence-before-side-effect, one inspect per cycle, retry schedule, same-ID-only safe retry, and indefinite block for unresolved/uninspectable IDs. A failed state persistence suppresses subsequent create/start/inspect/follow-on operation.
- Attached start/stream capture runs in a worker that alone owns transport and cannot mutate persistent state or release the durable attempt. The main poll loop remains nonblocking; an in-memory single-flight guard remains until worker termination even if inspect sees terminal status. Every persisted ID is inspected on the normal cycle independently of worker acknowledgment. Recovered running/terminal IDs are inspected, never reattached. Cancellation, connection cleanup and worker termination are bounded; late results are discarded. Existing exact same-ID retry eligibility and 409 reconciliation remain authoritative.
- Keep custom Docker attached-stream transport/parser behavior and limits, TCP/unix endpoints, HTTP body framing, DNS/setup bounds, cancellation, and worker lifecycle. No total Graphify command timeout.

## Image and deployment

Use pinned Go builder and Alpine runtime stages compatible with existing package versions. Runtime retains pinned Git and Docker CLI plus CA certificates; omit Python. Keep image/tag `registry.localdomain:5000/docker-watcher:latest`; exact Compose name `docker-watcher`; `.env`; named `/data`; restart policy, health formula/start period, resource limits, JSON log rotation, Watchtower exclusion, and `DOCKER_HOST=tcp://docker-lxc.localdomain:2375`. Do not add socket mounts or registry login.

## Verification architecture

- Standard Go test suite with deterministic fake HTTP servers/transports, fake executables, temp state files, scripted local TCP/unix peers, and injected time where needed.
- All test execution stays offline; no live daemon, registry, GitHub, or production Graphify interactions. Before implementation, freeze the detailed compatibility matrix from the approved plan, including config/log/state contracts, request/subprocess bounds, Compose/Loki/alert selectors. Establish attached transport feasibility only with local scripted peers and record evidence; never contact a live endpoint.
- Require at least 90% full-suite Go source-line coverage; run `go test ./...`, coverage, `go vet`, formatting and race checks where practical; validate Compose config offline. Test that failed state saves suppress later external operations, that the post-build target save precedes tag push, and that worker/inspect/recovery interleavings cannot overlap or reattach.
- Preserve current operational logs/alert contract; offline-check the literal `build failed`, Loki selector, and alert YAML's datasource UID placeholder/non-provisioned status. Do not add telemetry or provision Grafana. Operator validates actual Loki labels/alert routing only after authorized deployment; missing or broken live rule/datasource/default route blocks notification acceptance pending separate authorization and delivery test.
- Update requirements/design/tasks and development documentation; remove Python source/tests/dependencies, not a compatibility shim.

## Cutover safety

Operator retains the old image by digest/local tag, stops the old writer and verifies it is stopped, then inspects/backups the quiescent current `/data/state.json` before exclusively starting Go. Resume any saved Graphify ID only by inspecting that same ID. An uninspectable ID preserves state and pauses Graphify processing until independent proof of completion/nonexecution and separately authorized state repair; unrelated polling/build may proceed only if it cannot change the attempt or create conflicting Graphify work. Rollback stops Go first and retains current state by default; restore a pre-cutover snapshot only after reconciling all later build/retry/Graphify changes. If old image reference or state safety cannot be established, pause for operator recovery. No dual writers. No agent-led build, registry write, deployment, live Docker/Graphify operation, or SSH.

## Approved tradeoffs

- Keep the known Docker CLI build path rather than replacing proven semantics with an SDK.
- Use stdlib HTTP unless necessary offline-parity evidence justifies an additional dependency.
- Coverage gate is 90% full Go production source, with deterministic offline tests as the proof boundary.
- Runtime conversion only; no performance target, new observability, CI workflow, or alert provisioning is authorized.