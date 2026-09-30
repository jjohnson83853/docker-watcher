# Graphify refresh observability — Stage 0 research

Date: 2026-09-30  
Status: **PARTIAL — source/documentation research completed; deterministic Docker sandbox experiment blocked by unavailable local daemon.** No production endpoints, remote Docker contexts, Graphify refreshes, image changes, or graph data were accessed or modified. User authorization received for sandbox-only Stage 0 research; it does not authorize operational commands.

## Findings

### Docker exec output visibility

The watcher implementation creates execs with `AttachStdout=false` and `AttachStderr=false`, then starts with `Detach=true` (`graphify_refresh.py`, `exec_create`/`exec_start`). Docker Engine API documentation describes stream attachment at exec creation and says detached start returns immediately; `ExecInspect` returns process metadata including `Running` and `ExitCode`, not output. The documented API exposes no later endpoint for retrieving detached exec output. This makes post-hoc Docker output retrieval unlikely, but **runtime behavior was not experimentally verified here**.

- Docker Engine API v1.40: [exec create/start/inspect definitions](https://docs.docker.com/reference/api/engine/version/v1.40.yaml) (operations `ContainerExec`, `ExecStart`, `ExecInspect`).
- Project implementation: [`graphify_refresh.py`](../../../graphify_refresh.py), lines 230–256.

### Graphify 0.9.67 error and artifact behavior (source review)

Reviewed the official [`Graphify-Labs/graphify` `v0.9.67` source branch](https://github.com/Graphify-Labs/graphify/tree/v0.9.67):

- `graphify update` checks for a missing workspace path and emits the exact stderr prefix `error: path not found: ` before exiting 1 ([`cli.py`](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/cli.py#L2417-L2419)); when `_rebuild_code` returns false, it emits “Nothing to update or rebuild failed — check output above.” and exits 1 ([`cli.py`](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/cli.py#L2438-L2443)).
- The rebuild exception handler prints the exception text to stdout and returns false ([`watch.py`](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/watch.py#L2229-L2231)); other failure paths print to stderr. Therefore useful detail is CLI stream output, not an `ExecInspect` field. Exception/output text can contain paths or other sensitive values; do not forward it raw to Loki.
- `.rebuild.lock` contains the holder PID while a rebuild runs and is unlinked by the lock context manager on release ([`watch.py`](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/watch.py#L160-L215)). It is transient coordination state, not a durable failure record.
- Per-file AST extraction failures are tracked in memory and omitted/cleared from the `manifest.json` stamped set; the manifest can indicate an unstamped source, but does not preserve the underlying error cause ([`watch.py`](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/watch.py#L1795-L1805), [`save_manifest` call](https://github.com/Graphify-Labs/graphify/blob/v0.9.67/graphify/watch.py#L1962-L1973)). This is not evidence that every internal failure path lacks every possible artifact; the reviewed rebuild path exposes no general durable exception report.

Source review indicates Approach A's known artifacts are insufficient to classify a general `graphify update` exit code 1: at most they expose transient lock state or coarse per-file unstamped state, while failure details are emitted to stdout/stderr. This is a source-derived conclusion, not a runtime reproduction.

## Sandbox preflight and limitation

- Docker CLI exists; its selected context is `default` with endpoint `unix:///var/run/docker.sock`.
- `docker info` and `docker image ls` failed because that local daemon is unreachable.
- `dockerd`, `containerd`, `nerdctl`, `podman`, and `buildah` are unavailable. `graphify` is not installed locally. GitHub CLI is unauthenticated; public primary-source pages were available.
- Portainer read-only endpoint metadata lists `Services` (ID 3) and `docker-lxc` (ID 5), both active Docker endpoints, but neither is labelled as an isolated sandbox; groups/tags are absent. No Docker command was sent through Portainer to either endpoint. Consequently, no remote or production Docker environment was used for this experiment and no disposable container was created; detached-stream retention and Graphify 0.9.67 output/artifact behavior remain untested by execution.

## Leak review and recommendation

- Do not add raw stdout/stderr, raw exception strings, or arbitrary `GRAPHIFY_OUT` file contents to watcher events. Graphify emits exception text on its CLI stream; source paths may be present.
- The authorized Stage 0 experiment, if an isolated runtime becomes available, may run only against a copied disposable workspace with a separate temporary `GRAPHIFY_OUT`; verify the command cannot modify the copied source and keep/delete output per the leak controls. This experiment is blocked by the unavailable local Docker daemon.
- Any later Stage 4/T6 diagnostic capture is separate from Stage 0: it requires a post-research strategy decision, the approved SpecDD/runbook, and fresh explicit user authorization of the exact command/isolated target/window. If the error does not reproduce in isolation, retain uncertainty; do not use production graph data as a shortcut. B remains incomplete until separately authorized execution produces a sanitized evidence note.
- **Strategy decision (2026-09-30):** the user selected sanitized fixed-enum classification from an attached Docker exec stream; raw Graphify output must not enter Loki. The user explicitly requires preserving the current `tcp://docker-lxc.localdomain:2375` endpoint; the design therefore requires bounded hostname resolution rather than replacing the endpoint with an IP literal. This changes the confirmed refresh exec/logging contract. No implementation is authorized until the active contract amendment is Critic-reviewed and the user separately confirms requirements, design, and tasks. This selection does not authorize Docker experiments, production access, a production refresh, image build/push, or deployment.

## References

- Active watcher contract: [`.superpowers/sdd/graphify-refresh/design.md`](../graphify-refresh/design.md), §§1, 4, 6.
- Docker Engine API v1.40 `POST /exec/{id}/start`: `Detach=false` sets up an interactive session and produces `application/vnd.docker.raw-stream`; `Detach=true` returns immediately after starting. This documents the attached stream mechanism, not behavior after a dropped connection: https://docs.docker.com/reference/api/engine/version/v1.40.yaml (ExecStart).
- Observability plan: [`.omp/plans/graphify-refresh-observability.md`](../../../.omp/plans/graphify-refresh-observability.md).
