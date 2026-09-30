# Graphify Refresh Observability — Requirements

Status: **Critic-approved; requirements, design, and tasks separately confirmed by the user on 2026-09-30; dedicated implementation gate open; local implementation and verification complete.** This amendment is scoped to the existing Graphify refresh contract in `.superpowers/sdd/graphify-refresh/`. No live Docker/container operation, image build/push, deployment, or production Graphify operation is authorized by this amendment.

## 1. Goal

When Graphify’s workspace update exits nonzero, make a bounded, useful diagnostic classification available in docker-watcher’s existing structured logs so Loki can ingest it, without forwarding Graphify’s raw output or changing refresh scheduling/recovery semantics.

## 2. Functional requirements

- **FR-1 Output visibility:** docker-watcher MUST receive the stdout/stderr stream of its Graphify Docker exec. The current unattached, detached invocation cannot supply this stream.
- **FR-2 Safe classification:** On a verified nonzero terminal exit, emit `diagnostic_class` from the fixed enum `workspace_path_missing` | `graphify_rebuild_failed` | `unclassified` | `unavailable`. The first two values mean only that the corresponding approved signature was observed on stderr; they do not assert a verified root cause or Graphify version. `unclassified` means the complete, well-formed stream matched no approved signature. `unavailable` means capture is incomplete, malformed, scan-capped, errored, cancelled, absent after watcher restart, or not complete when terminal failure is observed. A finite connect/header or terminal stream-read timeout is unavailable; an idle read timeout while `ExecInspect` still reports running is retried and is not terminal. Never infer a cause from exit code alone.
- **FR-3 No raw diagnostic output:** Never write Graphify stdout/stderr, exception strings, paths, credentials, request payloads, or raw Docker response content to watcher logs, persisted refresh state, or Loki. Output is inspected transiently by the classifier and discarded.
- **FR-4 Loki path:** Emit the classification as a field on the existing structured `exec_failed` event. Do not add a new telemetry service, label, dashboard, alert, or log source.
- **FR-5 Lifecycle compatibility:** Preserve global single-exec serialization, persisted exec identity, nullable `ExecInspect.ExitCode` interpretation, retry/backoff, coalescing, restart recovery, and build/push isolation. The persisted attempt reserves the global single-flight slot before any attached start and retains it through the existing terminal inspect/retry transition, including pre-ID and in-flight-start phases. A per-attempt capture worker owns attached start and stream handling but never persisted state, retry/backoff, or the single-flight slot. Terminal inspect cancels the active worker; if inspect clears persisted state first, an in-memory guard blocks a new exec until worker termination, while build/push processing continues and late results are discarded. Worker completion never releases persisted state or authorizes retries. Do not reattach after restart to a running or terminal saved exec; retain same-ID retry only for verified `Running=false, ExitCode=null`. Stream-capture failure MUST NOT cause another Graphify exec or alter terminal success/failure semantics; emit at most one failure event per terminal nonzero exec.
- **FR-6 Bounded processing:** Parse Docker’s non-TTY multiplexed stdout/stderr stream incrementally with bounded memory. Handle fragmented headers/payloads, malformed frames, empty streams, oversized output, connection errors, and watcher restart. An unavailable/truncated/invalid capture maps to a fixed enum, not raw text.

## 3. Non-functional requirements

- **NFR-1 Privacy:** All emitted values are internal allowlisted enums; adversarial output containing paths, secrets, control characters, or forged JSON MUST never appear in logs.
- **NFR-2 Isolation:** Capturing output MUST NOT block image builds, tag pushes, or processing other repositories while the Graphify exec is running.
- **NFR-3 Determinism:** Docker stream demultiplexing, classification precedence, output truncation, connection loss, and restart fallback MUST be covered by deterministic tests without contacting Docker, Graphify, Loki, or production systems.
- **NFR-4 Minimal change:** No changes to Graphify configuration, mounts, network, image, graph data, retry policy, or container-log rotation.
- **NFR-5 Bounded connection setup:** Preserve the configured `DOCKER_HOST`, including `tcp://docker-lxc.localdomain:2375`. Hostname resolution MUST run in a bounded helper process. One monotonic deadline MUST cover resolution, connection, request send, and full response headers, including trickling headers; cancellation MUST terminate the resolver child or close the active socket. A timed-out setup is `unavailable`, not a Graphify command failure. Stream idle reads retry while ExecInspect reports running and do not create an overall command timeout.

## 4. Explicit decisions

- Selected strategy: sanitized classification, not raw output.
- The refresh command continues to execute inside `graphify-mcp`; docker-watcher is the Docker Engine client that starts and observes it. Loki cannot see exec output unless the watcher explicitly receives and logs an approved derived field.
- Existing refresh design §§1 and 4 require amendment before implementation because stream capture changes Docker exec attachment/start behavior and permits a new safe event field.

## 5. Out of scope

Root-cause remediation, raw output logging, additional Grafana dashboards/alerts, operational Docker experiments, production graph mutation, image build/push, and deployment.
