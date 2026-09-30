# Graphify Refresh Observability — Progress Ledger

Status: **Critic-approved; all three user checkpoints confirmed; dedicated implementation gate open; local implementation and verification complete.** No live Docker/container operation, image build/push, deployment, or production Graphify update has occurred for this amendment.

## Research

- Existing watcher exec creation leaves stdout/stderr unattached and start is detached; Graphify runs inside `graphify-mcp`, not in docker-watcher. Loki sees only watcher-emitted logs through the existing source path.
- Graphify v0.9.67 `cli.py` provides a stable missing-path prefix and a generic rebuild-failed summary. These support only bounded classes; the generic class does not prove the underlying cause.
- Research note: `research.md`. Proposed safe strategy selected by user: sanitized fixed-enum classification, never raw output.

## Critic reviews

- First-draft Critic review: **REVISE**. Findings: attached `exec_start` worker ownership/start ordering could block watcher work or permit a duplicate start; EOF/error/timeout/cancellation and completion handoff were underspecified; post-cap behavior was ambiguous; signature names/version provenance and enum vocabulary needed precision; active contract sections and interleaving acceptance were not explicit.
- Amendments applied to the draft package: reserve the existing persisted single-flight attempt before attached start; worker alone owns start/stream and cannot mutate persistent state or slot; main loop observes start/capture handoffs without waiting; specify EOF/frame/error/read-timeout/cancel behavior, drain-and-discard after scan cap, late-result discard, four-value enum, and signature-observed-not-cause semantics; name active contract sections and concurrency acceptance tests.
- Checkpoint resolution for the draft: avoid a runtime image-version dependency by labeling only observed stderr signatures, not causes; after the scan cap drain-and-discard to EOF instead of closing a still-running exec. The main loop closes the stream only after `ExecInspect` has already confirmed terminal status, or process teardown on watcher shutdown. No container experiment was used.
- Second Critic review: **REVISE**. Findings: a stalled connection/header wait could retain the worker guard indefinitely; cap counting (stdout/stderr payload versus framing headers) was ambiguous; idle read timeout conflicted with `unavailable`; the active-contract text was not present in the review diff.
- Amendments applied: finite connect/header/read timeouts use existing request-timeout configuration; a cancellation control registers/closes the connection and is checked around I/O; the main loop retains the worker guard until thread exit without blocking build/push; tests cover blocked pre-header cancellation and later eligible refresh. `SCAN_CAP_BYTES` now counts combined stdout+stderr payload bytes only, excluding frame headers; active refresh requirements/design/tasks now include the actual proposed contract text, still pending approval.
- Third Critic review: **REVISE**. Findings: per-operation timeouts did not establish a total bound against stalled hostname resolution or trickling response headers; the confirmed active contract lacked the worker guard after terminal inspect; shutdown wording implied orderly worker cleanup; the exact Graphify signature source line was not cited.
- Amendments applied: preserve `tcp://docker-lxc.localdomain:2375`; require bounded hostname resolution and one monotonic DNS/connect/request/full-header deadline; add cancellation, worker-exit guard, nonblocking build/push, and late-result discard to the active requirements/design/tasks; clarify process-exit limits; cite the exact `cli.py` stderr line. At that review point, user confirmation of requirements/design/tasks remained pending; confirmations are recorded below.
- Final Critic review of the complete amended diff: **APPROVE**. No remaining findings. The verdict approves the proposed design only; implementation remains gated by the three separate user confirmations and dedicated amendment gate.

## User checkpoints

- Strategy selection: sanitized classification selected by user.
- Requirements confirmation: **confirmed by user (2026-09-30)** after final Critic APPROVE.
- Design confirmation: **confirmed by user (2026-09-30)** after final Critic APPROVE.
- Tasks confirmation: **confirmed by user (2026-09-30)** after final Critic APPROVE.

## Implementation gate

- `.superpowers/sdd/GATE_OPEN` for the existing graphify-refresh specification does not approve this material exec/logging contract amendment.
- Observability implementation gate: **open** after Critic APPROVE and all three separate user confirmations (requirements, design, tasks). Local implementation is complete; production Graphify operation remains unauthorized.
  
## Local implementation and verification

- Implemented attached non-TTY stream capture in `graphify_stream.py` and integrated it with the serialized lifecycle in `graphify_refresh.py`. Uses only fixed diagnostic classes; raw stream bytes remain outside watcher logs and durable state. Incomplete/malformed HTTP framing and failed terminal-state persistence are handled as unavailable/not-persisted without classifying truncated output or releasing the saved attempt. Resolver-child IPC failures are suppressed without child tracebacks.
- Preserved `tcp://docker-lxc.localdomain:2375`; hostname resolution runs in a bounded spawned process, including bounded/cancellable process startup. Timeout/abandonment cleanup is serialized; late resolver children are terminated and reaped before the startup slot is released. Terminal cancellation signals resolver termination without waiting for reaping; the capture/startup worker owns reaping.
- Verification: full `pytest` suite passed (118 tests); full-suite source-line coverage **90.71%**. Standalone `test_app.py` passed under the project venv.
- Current local TCP attached-stream smoke exercised spawned DNS resolution, strict HTTP/1.1 status parsing, complete chunked body/trailer validation, and the fixed `workspace_path_missing` result. The smoke output contained only that fixed class, not the raw payload.
- No live Docker container experiment, Docker image build/push, deployment, restart, or production Graphify update was performed. Live acceptance safeguards in `.superpowers/sdd/graphify-refresh/tasks.md` remain required.
