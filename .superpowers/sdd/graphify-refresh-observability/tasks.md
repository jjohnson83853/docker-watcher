# Graphify Refresh Observability — Tasks

Status: **Critic-approved; requirements, design, and tasks separately confirmed by the user on 2026-09-30; dedicated implementation gate open; local implementation and verification complete.** No live Docker/container operation, image build/push, deployment, or production Graphify operation is authorized by these tasks.

## 1. Design gate

- Critic reviews this saved requirements/design/tasks package and the active-contract amendments; apply exact required amendments and re-review.
- Before implementation, amend the active `.superpowers/sdd/graphify-refresh/` contract that currently prescribes unattached/detached execution: `design.md` §1 (exec attachment flags, detached start, and global exec lifecycle), §4 (safe event allowlist), and the corresponding exec-attachment/serialization requirements and tasks. Record the specific amended anchors in `progress.md`.
- Record Critic verdicts and the user’s separate requirements, design, and tasks confirmations in `progress.md`.
- Open a dedicated observability gate marker only after all confirmations. The existing `graphify-refresh` gate marker does not approve this contract change.

**Acceptance:** The named active-contract sections explicitly permit attached output capture, its fixed classification field, worker ownership/termination, and failure/restart fallbacks; Critic reviews/amendments and three separate user checkpoints are recorded before implementation.

## 2. Test-first Docker stream and classifier

- Add deterministic tests before implementation for non-TTY multiplexed frames split across arbitrary read boundaries, interleaved stdout/stderr, empty stream, invalid frame type/reserved bytes/oversized frame, partial header/payload at EOF, transport loss, HTTP Content-Length truncation and chunked trailer termination, connection/header timeout, idle read timeout while still running, cap boundaries, cap then drain, and terminal-inspect cancellation. Verify `SCAN_CAP_BYTES` counts combined stdout+stderr payload bytes only; headers do not count.
- Test malformed field names, field-value control bytes in headers/trailers, and signed, prefixed, or underscored chunk-size tokens as unavailable framing; force an idle socket timeout during trailer parsing, then verify valid trailer completion resumes with partial state intact.
- Add consumer-visible classifier cases for the exact stderr signatures `workspace_path_missing` and `graphify_rebuild_failed`, unmatched complete capture (`unclassified`), and incomplete capture (`unavailable`). Verify framing/chunk boundaries, precedence, stdout exclusion, and that the names assert signatures only, not causes.
- Test the concurrency interleavings: attached start blocked before response headers while another repo builds/pushes and a second refresh is requested; terminal inspect cancels the blocked start, its worker exits within the configured request timeout, then a later eligible refresh proceeds; rejected and transport-failed start handoffs; EOF before/after inspect; partial header/payload at EOF; idle read timeout; worker exception; cap reached with and without a prior signature; late worker completion after the single failure event; restart with persisted `start_requested`; nullable inspect running→terminal and terminal while worker pending. Also prove terminal success cancels/discards pending capture without changing success handling.
- Force the resolver-start completion/timeout interleaving at the deadline and prove the child is terminated and reaped before the startup slot is released. Prove terminal inspect signals resolver termination without waiting for child reaping.
- Repeatedly time out resolver startup and attempt a retry while the first `Process.start()` remains blocked; prove the later attempt creates no second starter until the first helper finishes and cleans up its child.
- Test resolver-process startup and DNS resolution against the same monotonic setup deadline; a stalled startup or resolver must be abandoned and any late-started child terminated. Preserve `tcp://docker-lxc.localdomain:2375` unchanged. Use controlled fake TCP sockets to prove fragmented/trickling HTTP response headers cannot extend that deadline, and verify cancellation closes the socket or terminates the child before the in-memory worker guard releases.
- Reject malformed HTTP status lines before publishing `started`; include malformed version, signed/short status code, and missing reason separator cases.
- Add adversarial tests proving paths, credentials, exception text, control characters, forged JSON, signature-like stdout, and resolver-child IPC failures never appear in logs, child stderr, or durable state.

**Acceptance:** Tests fail before implementation and pass with only the fixed four-value enum; they prove one worker/start at a time, no build/push blocking, no duplicate start, bounded parser memory, one failure event per terminal nonzero exec, and no raw diagnostic leak. No live Engine, Graphify, Loki, or production service is contacted.

## 3. Attach and classify Graphify exec output

- Amend `DockerClient.exec_create` to request non-TTY attached stdout/stderr. Persist exec ID and `start_requested` before delegating attached `exec_start` to the per-attempt worker. Keep the global slot owned by the persisted main-loop attempt; the worker never releases it.
- Implement the worker handoff exactly as design §3: nonblocking start status, stream/socket ownership in worker, bounded frame parser, terminal record in `finally`, per-read timeout/cancellation, drain-and-discard after `SCAN_CAP_BYTES`, and no worker creation for recovered running/terminal execs. Retry the same saved ID only under existing inspect/deadline rules after a prior worker has ended.
- Classify only the approved stderr signatures from design §2. Do not version-gate or assert causes. On verified nonzero `ExecInspect`, append one allowlisted `diagnostic_class`; if capture is incomplete or not yet complete, use `unavailable`. Discard any late result after that event.
- Keep raw bytes out of state, exceptions, normal logs, and diagnostic error reporting. Update the active refresh requirements/design/tasks and root operational design documentation to reflect the approved contract before opening the implementation gate.

- **Acceptance:** Existing lifecycle/state behavior remains unchanged; tests prove the worker cannot block image-build/tag-push processing, no second worker/start occurs while the prior start is active, cancellation during connect/header wait releases the worker guard within the configured request timeout, a later eligible refresh resumes after worker exit, a terminal event appears at most once, terminal cancellation/shutdown releases the worker/socket, the persisted single-flight slot cannot be released by a worker, and all four enum outcomes follow design §2/§3.

## 4. Verification and review

- Run focused stream/classifier and lifecycle tests, then `python3 test_app.py` and the repository’s normal full suite; measure full-suite source-line coverage and require at least 90%.
- Run a throwaway fake Docker Engine smoke scenario exercising attached stream frames, one recognized nonzero failure, and one malformed/disconnected stream. No actual Docker containers or Graphify commands.
- Run code-reviewer and security-reviewer on the patch; resolve CRITICAL/HIGH findings.
- Report tests, coverage, smoke result, and review findings. Do not build/push images, deploy, or read production container logs unless separately requested/authorized.

**Acceptance:** All local checks pass; no raw diagnostic output leaks; no Docker container experiment or production operation occurs. Commit/push only after verification and the applicable user commit checkpoint.
