# docker-watcher Go conversion — Requirements

Status: **confirmed**. This is a runtime-language conversion, not a product expansion. The approved implementation and safety baseline is recorded in `.superpowers/sdd/docker-watcher-go-conversion/requirements.md` and `.omp/plans/docker-watcher-go-conversion.md`.

## Goal

Replace the Python runtime with Go while preserving repository polling and image publication, durable state, Graphify workspace refresh, logs, environment variables, and deployment contracts. Do not add Docker event watching, inbound endpoints, multiple replicas, telemetry, registry authentication, or dual-run behavior.

## Required behavior

- Enumerate authenticated owner repositories from GitHub at 100 per page; filter owned repositories, report empty/error outcomes, use the selected/default branch, and detect a root `Dockerfile`.
- Skip repository detail requests when saved `pushed_at` is unchanged. Compare branch-tip SHA with saved SHA before building.
- Shallow clone, use `git rev-parse HEAD` as authoritative SHA, remove `.git`, run the pinned Docker CLI build, then push `latest` followed by the seven-character SHA tag. Continue after individual repository failures.
- Immediately after a successful build and before refresh scheduling or tag pushes, persist that repository's Graphify target. A failed durable save blocks pushes and dependent external actions.
- Preserve required `GITHUB_USERNAME` and `GITHUB_TOKEN`; optional `BRANCH`; defaults `REGISTRY_URL=registry.localdomain:5000`, `STATE_FILE=/data/state.json`, `POLL_INTERVAL_SECONDS=300`, `BUILD_TIMEOUT_SECONDS=1800`, `HEARTBEAT_FILE=/data/heartbeat`, and `GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS=10`; preserve `DOCKER_HOST` and `DOCKER_BUILDKIT=0` behavior.
- Bound every subprocess. Do not expose the PAT in output, build context, or rendered errors. Build/push diagnostics are limited to the final 4000 characters; clone/revision errors do not expose command arguments or output.
- Preserve JSON stdout event/field meanings, including the literal `build failed`; never log raw Graphify output, paths, response bodies, or credentials.
- Preserve state schema, legacy bare SHA acceptance, unknown keys, Graphify target/completed/failure/retry data, and atomic same-directory replacement. Conversion must not cause rebuild/replay or discard retry state.
- Preserve Graphify's `graphify update /workspace` command and `graphify-mcp` environment, API version negotiation, one workspace operation at a time, persistence-before-side-effect, one inspection per cycle, 60-to-1800-second exponential retry, and indefinite blocking for unresolved IDs. Retry may only start the same saved ID after inspect reports `Running=false, ExitCode=null` and the persisted deadline has elapsed; reconcile HTTP 409 by inspecting that ID.
- Preserve attached Docker TCP/unix transport and bounded HTTP framing, multiplexed stream parsing, stdout/stderr distinction, 1 MiB frame and 64 KiB aggregate scan caps, bounded setup/DNS, cancellation/cleanup, fixed diagnostics, and no command-wide Graphify timeout. Worker owns attach/capture; it cannot change durable state or release the attempt slot. Inspect recovered IDs; never reattach them.
- Keep sequential cycles and heartbeat-on-cycle behavior. Preserve Compose name, image, `.env`, `/data`, restart policy, health threshold/start period, resource limits, JSON log rotation, Watchtower exclusion, and TCP Docker proxy endpoint. No socket mount or registry auth.
- Remove Python modules, dependencies, tests, pytest configuration, and runtime; no compatibility shim.

## Verification and operating boundary

Tests must be deterministic and offline: no GitHub, remote Git, Docker daemon, registry, deployment, or production Graphify access. Full Go source-line coverage target is at least 90%; the orchestrator owns project-wide validation. Operators own the exclusive cutover/rollback and all live operations. Never run Python and Go writers concurrently against state, tags, or Graphify work.
