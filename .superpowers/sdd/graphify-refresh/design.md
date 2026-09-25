# docker-watcher Graphify Refresh — Design

Status: **draft — awaiting requirements checkpoint.** Implements `requirements.md`; this design does not authorize implementation.

## 1. Contract discovery gate

No Graphify endpoint, command, auth scheme, workspace identifier, or completion behavior is assumed. Before choosing the client implementation, inspect the deployed Graphify interface and watcher deployment to establish: (1) the refresh request and auth; (2) how each watched repo maps to the workspace Graphify already mounts; (3) whether that mount follows the current working tree; (4) whether refresh is synchronous or returns an accepted job; (5) whether job lookup exposes terminal status; and (6) network and timeout constraints. Record evidence and the selected outcome branch here before implementation. If an interface or mount cannot be verified, stop before implementation and report the missing prerequisite rather than inventing a request.

The source is Graphify's existing mounted workspace, never the temporary clone used by docker-watcher. The user accepts that edits after a Git push and before build completion may be indexed instead of the pushed revision. Do not add revision snapshots, clone transfer, workspace synchronization, or pinning for this race.

## 2. Integration boundaries and state

Keep Docker build success observable separately from tag publication. Persist a per-repository refresh intent immediately after a successful build and before the first Graphify call. The refresh subsystem must not use the `BuildError` path. Graphify call, decoding, and job-status exceptions are caught per repository, logged using the safe event contract, and cannot stop remaining tag pushes or repositories.

Extend the existing atomic watcher state in `/data` with one refresh entry per repository. Preserve prior state compatibility. An entry carries the latest target generation, failure count (number of observed failures), next eligibility, last safe outcome, and where needed a pending-job record and dirty/newer-generation marker. Each submission also has a stable local request key and attempt-start timestamp persisted before invocation; send or use the key only if contract discovery proves caller-key support. Keep image-build state and refresh lifecycle distinct so later push failure cannot erase a refresh intent.

Every state transition that enables an external action must be durable first. If persistence fails, suppress the dependent Graphify submission, poll, or follow-on; emit a safe `persist_failure`/specific transition event that does not claim persistence. Existing single-replica state and atomic-write behavior remain the storage foundation.

## 3. Lifecycle and scheduling

- **New successful build, no retry/job:** persist a new generation and request metadata, then issue the first refresh immediately in that same cycle. A failure to request refresh does not block image pushes.
- **Pending retry, not in flight:** update its target to latest workspace content. Do not invoke immediately or change its due time/backoff; the already scheduled attempt refreshes current mounted content.
- **Observable job in flight:** persist a newer/dirty generation when another build succeeds, but do not overlap jobs. A submission spends no job-poll budget. Poll each repo at most once per normal watcher cycle on the existing `POLL_INTERVAL_SECONDS` cadence.
- **Terminal job success:** clear only the generation indexed by that job, and only after the terminal transition is persisted. If a newer generation is dirty, follow it at most once, only after persistence. The first poll for a follow-on job begins on a later cycle if the cycle's one-poll budget was used to settle the prior job.
- **Job failure:** retain the latest target and schedule retry with the known-failure clock. Poll exceptions and nonterminal results do not create duplicate submissions.
- **Sync completion:** remove the matching pending generation after success is known and the transition is persisted.
- **Accepted async without status lookup:** count acceptance as requested-trigger success only, remove its retry intent after persistence, and log/document that indexing completion is unconfirmed.

Retry indefinitely with a 60-second initial delay, doubling after each observed failure up to 30 minutes. For observed failures, set `next_eligibility = failure_observed_at + delay_for_this_failure`; failure count increments only for observed failures. Separately, before a submission persist a conservative crash-recovery eligibility `attempt_start + delay_for_next_failure` (60 seconds initially; subsequent delay is the next doubled delay, capped at 30 minutes). This fallback covers process death before the result is durably observed and is not the known-failure clock. Coalescing never resets either clock. An ambiguous/timeout outcome is not success: persist it as an observed failure and retain its local request key. If the verified contract supports reconciliation/idempotent replay, reuse the same key for that ambiguous attempt. Otherwise wait until eligible and create a new attempt with a new key. Never claim a stronger outcome than the contract exposes.

## 4. Logging and secret handling

Emit structured `graphify_refresh` events for detected failure conditions; use distinct internal outcomes including `invoke_error`, `timeout_unconfirmed`, `job_failed`, `poll_error`, `persist_failure`, `job_id_persist_failure`, `dirty_marker_persist_failure`, and `terminal_persist_failure`. The final implementation may refine event shape to the verified contract but must preserve the safe-value rules:

- Repo/workspace identifiers come only from configured values after canonical bounded validation; otherwise log a fixed safe hash or `unknown`.
- External statuses map to a fixed internal enum. HTTP status is logged only as an integer 100–599, otherwise `unknown`.
- Log job IDs only when they pass strict bounded opaque-token validation; otherwise omit or hash.
- Never log credentials, raw response bodies, request payloads, raw exception strings/tracebacks, raw hosts/paths, or unvalidated external status strings.
- Graphify-only events never start with the existing literal `build failed` prefix.
- Report retry delay/next eligibility only when it was successfully persisted; state-write failures explicitly indicate `not_persisted`.

## 5. Configuration and deployment

Use the minimum configuration needed by the verified interface, exposed through existing watcher deployment/environment files. Include a bounded Graphify call timeout. Add a runtime dependency only if contract discovery demonstrates the stdlib is insufficient. Keep the Graphify workspace mount on Graphify; add no watcher mount absent evidence of a requirement. Verify connectivity from the watcher container context without sending secrets to logs.

## 6. Compatibility, verification, and acceptance

Keep existing image build/push behavior and state readable. Extend deterministic `test_app.py` coverage for build-success trigger ordering, push-failure retention, continued tag pushes after Graphify failure, no trigger on skipped/unchanged/failed builds, durable restart retries and two-clock/backoff semantics, retry coalescing without deadline reset, in-flight generation/no-overlap behavior, one-poll-per-cycle limit, terminal transitions and persistence failures, and secret-free structured events. Test contract-dependent branches using a fake Graphify client and a deterministic clock; do not call homelab services in unit tests.

Separately perform live acceptance against the verified Graphify deployment: verify mount mapping/currentness, watcher-to-Graphify connectivity and auth, real refresh request, and completion/status interpretation. Do not mark end-to-end acceptance complete if the deployment contract cannot be exercised; record the exact environmental prerequisite or observable limitation.
