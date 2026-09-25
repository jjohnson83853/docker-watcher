# docker-watcher Graphify Refresh — Tasks

Status: **draft — awaiting user requirements checkpoint.** Read `requirements.md` and `design.md` first. After requirements confirmation, task 1 must run before the final design/tasks checkpoints; any material update requires fresh Critic review and renewed user confirmation. Implementation is blocked until all three user checkpoints, Critic verdicts, and project gate are recorded.

## 1. Resolve deployed contract (after requirements checkpoint; before design/tasks checkpoints)

- Inspect Graphify's deployed interface, documentation, auth, repo/workspace mapping, mount scope/currentness, refresh semantics, job-status observability, ambiguity reconciliation/identification, network reachability, and timeout constraints.
- Record evidence and select the synchronous, observable-async, or unobservable-async branch in `design.md`. If accepted acknowledgments have no status API, present the explicit tradeoff: block later submissions indefinitely unless status is safely resolved, or permit successive requests that may overlap. Obtain the user's decision before selecting the latter branch.
- Submit material design/tasks changes from discovery to Critic again and obtain renewed user confirmations before implementation.

**Acceptance:** Design identifies a verified request and source mapping, accurately states success/ambiguity behavior, and records the accepted post-push/pre-build workspace race. An accepted-only async branch cannot be selected without explicit approval of the overlap tradeoff. If the contract cannot be verified, implementation remains blocked.


## 2. Persist refresh lifecycle independently from image publication

- Make successful Docker build completion observable separately from tag pushes.
- Add a durable per-repository refresh intent to existing watcher state immediately after build success and before submission; retain it across later tag failures.
- Store local request key, attempt-start/fallback deadline, generation, observed-failure count, retry eligibility, safe last outcome, and pending-job/dirty-generation data as required by the verified contract.
- Preserve compatibility with existing state, atomic persistence, and image/push behavior.

**Acceptance:** Each logical successful build generation gets one durable intent if its post-build state commit completes, despite later push failure; a crash before that commit may lose the trigger as documented. Failed/skipped/unchanged builds do not. A Graphify error does not prevent tag pushes or processing another repo. No intent is acted on before its state write succeeds.

## 3. Implement per-repo refresh scheduling and outcomes

- Invoke immediately only when there is no pending retry, tracked in-flight job, or unresolved potentially accepted request.
- Coalesce newer builds into a pending retry without an extra immediate call or due-time/backoff reset.
- Prevent overlapping observable jobs; persist dirty generations; poll at most once per repo per normal watcher cycle; clear only the indexed generation after durable terminal success; follow newer content at most once after persisting that transition.
- Implement indefinitely retrying 60s/doubling/30m-cap backoff for safely retryable observed failures. Separate failure-observed deadlines from attempt-start crash-recovery fallback deadlines. Retain/reuse the ambiguous attempt key only for verified reconciliation/idempotent replay. If an async request may have been accepted and cannot be reconciled or identified, persist an unresolved per-repo intent that blocks submission for all generations, including after restart and after its eligibility deadline. Coalesce later builds into its durable latest target without resetting clocks. Resume only after verified safe resolution or explicit user decision to accept a weaker no-overlap guarantee; elapsed time alone never authorizes replay.
- Retry poll errors on the next normal watcher cycle without submitting another job. Retain nonterminal jobs indefinitely, at most one poll per repo/cycle, without inventing a timeout or overlapping a replacement.
- Implement verified synchronous, observable-async, and/or accepted-only semantics without claiming indexing completion beyond available evidence. Isolate invocation, decoding, polling, and persistence errors per repo.

**Acceptance:** Deterministic clock/client scenarios prove timing, restart persistence, coalescing, no overlap, poll bounds, terminal behavior, ambiguity handling, and persistence-failure suppression. Include generation A invocation timing out with possible async acceptance and no reconciliation, followed by successful generation B durably coalesced with no B invocation; include crash-after-send, restart, elapsed deadline, and no replay when acceptance cannot be reconciled.

## 4. Add safe events, configuration, and operational documentation

- Emit structured `graphify_refresh` failure events using only the validated/sanitized values and fixed vocabulary in the design; never reuse `build failed` for Graphify-only errors.
- Add only verified endpoint/auth/timeout and any necessary workspace-mapping configuration to `.env.template` and `docker-compose.yml`; add dependencies only if demonstrated necessary. Keep Graphify's existing workspace mount; do not duplicate it in watcher.
- Document the verified contract, retry and completion semantics, workspace race, accepted-only limitation if applicable, and operational configuration.

**Acceptance:** Tests prove secrets, raw payloads/responses/exceptions, invalid identifiers, invalid status values, and unsafe job IDs do not appear in events. Valid values are bounded and safe. Configuration is explicit and secrets remain external.

## 5. Verify application and live integration

- Extend `test_app.py` with consumer-visible behavior checks: build success and push-failure retention; no trigger for skipped/failed/unchanged builds; first-attempt ordering; first Graphify failure does not stop pushes; retry deadlines/backoff and simulated restart; pending-retry coalescing; in-flight dirty generation and no overlap; one poll per repo/cycle; sync/async outcome branches; exception isolation; persistence failures; sanitized events.
- Run the targeted test suite and a throwaway integration smoke against the verified Graphify contract from the watcher runtime context. Observe workspace mapping/currentness, auth/connectivity, invocation result, and status interpretation.

**Acceptance:** Local tests pass. A live smoke demonstrates the configured end-to-end request and correct outcome semantics before end-to-end acceptance or implementation sign-off; if unavailable, record the precise prerequisite and leave those statuses open. No test contacts production Graphify.
