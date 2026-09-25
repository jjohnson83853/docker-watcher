# docker-watcher Graphify Refresh — Tasks

Status: **draft — awaiting requirements and design checkpoints.** Read `requirements.md` and `design.md` first. Implementation is blocked until the three user checkpoints, Critic verdicts, and project gate are recorded.

## 1. Resolve deployed contract (read-only; before implementation)

- Inspect Graphify's deployed interface, documentation, auth, repo/workspace mapping, mount scope/currentness, refresh semantics, job-status observability, network reachability, and timeout constraints.
- Record evidence and select the synchronous, observable-async, or unobservable-async branch in `design.md`. If the endpoint, mount, or usable invocation cannot be verified, stop before implementation and report the missing prerequisite.

**Acceptance:** Design identifies a working request and source mapping, accurately states what constitutes success, and records the accepted post-push/pre-build workspace race.

## 2. Persist refresh lifecycle independently from image publication

- Make successful Docker build completion observable separately from tag pushes.
- Add a durable per-repository refresh intent to existing watcher state immediately after build success and before submission; retain it across later push failures.
- Store local request key, attempt-start/fallback deadline, generation, observed-failure count, retry eligibility, safe last outcome, and pending job/dirty-generation data as required by the discovered contract.
- Preserve compatibility with existing state, atomic persistence, and existing build/push semantics.

**Acceptance:** Successful build triggers despite a later push failure; failed/skipped/unchanged builds do not. A Graphify error does not prevent tag pushes or processing another repo. No intent is acted on before its state write succeeds.

## 3. Implement per-repo refresh scheduling and outcomes

- Invoke immediately only when there is no pending retry or in-flight job.
- Coalesce newer builds into pending retry without an extra immediate call or due-time/backoff reset.
- Prevent overlapping observable jobs; persist dirty generations; poll at most once per repo per normal watcher cycle; clear only the indexed generation after durable terminal success; follow newer content at most once after persisting that transition.
- Implement indefinitely retrying 60s/doubling/30m-cap backoff. Separate failure-observed retry deadlines from attempt-start crash-recovery fallback deadlines. Retain/reuse the ambiguous attempt key only for verified reconciliation/idempotent replay; otherwise mint a new key for a later attempt.
- Implement verified synchronous, observable-async, and/or accepted-only semantics without claiming indexing completion beyond available evidence. Isolate invocation, decoding, polling, and persistence errors per repo.

**Acceptance:** Deterministic clock/client scenarios prove timing, restart persistence, coalescing, no overlap, poll bounds, terminal behavior, ambiguity handling, and persistence-failure suppression.

## 4. Add safe events, configuration, and operational documentation

- Emit structured `graphify_refresh` failure events using only the validated/sanitized values and fixed vocabulary in the design; never reuse `build failed` for Graphify-only errors.
- Add only the verified endpoint/auth/timeout configuration to `.env.template` and `docker-compose.yml`; add dependencies only if demonstrated necessary. Keep Graphify's existing workspace mount; do not duplicate it in watcher.
- Document the verified contract, retry and completion semantics, workspace race, accepted-only limitation if applicable, and operational configuration.

**Acceptance:** Tests prove secrets, raw payloads/responses/exceptions, invalid identifiers, invalid status values, and unsafe job IDs do not appear in events. Valid values are bounded and safe. Configuration is explicit and secrets remain external.

## 5. Verify application and live integration

- Extend `test_app.py` with consumer-visible behavior checks: build success and push-failure retention; no trigger for skipped/failed/unchanged builds; first-attempt ordering; first Graphify failure does not stop pushes; retry deadlines/backoff and simulated restart; pending-retry coalescing; in-flight dirty generation and no overlap; one poll per repo/cycle; sync/async outcome branches; exception isolation; persistence failures; sanitized events.
- Run the targeted test suite and a throwaway integration smoke against the verified Graphify contract from the watcher runtime context. Observe workspace mapping/currentness, auth/connectivity, invocation result, and status interpretation.

**Acceptance:** Tests pass; live smoke demonstrates the configured end-to-end request and correct outcome semantics, or records a precise unavailability blocker without claiming success. No test contacts production Graphify.
