# docker-watcher Go conversion — Progress Ledger

Status: **requirements, design, and tasks confirmed by user; scoped implementation authorization recorded; saved SpecDD critic review ACCEPT.** Implementation remains blocked until gate artifacts are committed and this conversion gate is opened.

## Approval ledger

- Requirements checkpoint: **confirmed by user** — preserve existing service contracts; convert runtime only.
- Design checkpoint: **confirmed by user** — retain Docker CLI build path, stdlib HTTP, Graphify lifecycle/stream parity, pinned multi-stage image, offline tests, ≥90% Go coverage.
- Tasks checkpoint: **confirmed by user** — six phases, including user-owned operational cutover.
- Critic review history of `.omp/plans/docker-watcher-go-conversion.md`: initial draft **REJECT**; first factual revision **REVISE**; second revision **ACCEPT**; amendments applied.
- Initial critic review of this saved SpecDD package: **REVISE**. Findings and amendments are recorded in this ledger's history below.
- Final saved SpecDD review: **ACCEPT**, with no critical/major findings. Minor notes were resolved by completing the exact log inventory and retaining the artifact-commit gate.
- Implementation authorization: after being told checkpoint approval alone did not authorize implementation and a separate implementation request was needed, user replied **“continue”**. In that context, this is the separate instruction to proceed with the approved conversion after critic review and gate-artifact commit. Scope is offline Go implementation only; no live Docker, image build/push, deployment, registry writes, SSH, or alert provisioning.

## Reviews and amendments

- Initial saved-SpecDD critic verdict: **REVISE** — stale implementation status; Phase 0 omitted compatibility inventory and local transport evidence; build-target save ordering omitted; attached-worker/recovery invariants compressed; operator stop/snapshot sequence unclear; offline alert and Phase 4/5 acceptance evidence incomplete.
- Applied: explicit contextual authorization supersedes planning-only ledger; exact target-persist-before-tag-push contract and failure/crash tests; worker-only attached start, nonblocking poll, slot guard until worker exit, inspect-authoritative recovery/no reattachment, bounded cleanup; stop/verify old writer before quiescent state backup; uninspectable ID preserves state and pauses Graphify; alert prefix/selector/datasource and delivery acceptance conditions; Phase 4 PR/test-report criteria.
- Final critic verdict: **ACCEPT**. Critic's minor note required a concrete event/field inventory and no gate opening before artifact commit; both are now reflected below and in `tasks.md`.
- Approved plan's opening planning-only status is superseded by its updated approval ledger, which records the later contextual user instruction and its limited scope.

## Frozen compatibility baseline and feasibility

- The approved plan's behavior-parity matrix and `requirements.md` record environment defaults, state structures, Graphify transitions, request/subprocess/stream bounds, Compose settings, alert selector, and acceptance constraints.
- Event inventory from current `app.py`: `github_api_error` (`status`,`url`); `repos_found` (`count`, including zero); `build_started` and `build_success` (`repo`,`branch`,`sha` short prefix, `image`); `build failed` (`repo`,`branch`,`exit_code`, optional bounded `output_tail` or `error_type`); `graphify_refresh_module_unavailable`; `graphify_refresh` with `outcome=persist_failure`,`not_persisted=true`; `poll_cycle_failed` (`error_type`).
- Graphify emits `event=graphify_refresh`, with outcomes `exec_create_error`, `exec_start_unconfirmed`, `exec_running`, `exec_failed`, `exec_succeeded`, `exec_inspect_error`, `unresolved_exec`, `persist_failure`, and `retry_scheduled`; only bounded `repo(s)`, validated `exec_id`, HTTP `status`, `error_type`, `reason`, `not_persisted`, `retry_delay`, `diagnostic_class`, and `exit_code` fields may be emitted as applicable. The literal `build failed` remains an alert interface.
- Existing `test_graphify_stream_socket.py`, `test_graphify_stream_contract.py`, and `test_graphify_stream_frames.py` use local fakes/scripted peers for fragmented HTTP status/header/body, chunked framing/trailers, TCP/unix paths, frame parsing, rejection, caps, cancellation, and cleanup. Previously recorded local TCP smoke exercised spawned DNS resolution, strict HTTP/1.1 status parsing, complete chunked body/trailer validation, and fixed `workspace_path_missing` classification without exposing raw payload. No live endpoint was contacted. A narrow stdlib transport is feasible; no SDK is justified by evidence.

## Gate

- The root `.superpowers/sdd/GATE_OPEN` belongs to a different specification and does not open this conversion gate.
- Conversion gate: **closed pending gate-artifact commit**. Critic ACCEPT, user checkpoint confirmations, scoped implementation instruction, baseline, and local transport feasibility are recorded.
- After committing the four conversion SpecDD files, update the machine gate marker for this workspace before implementation dispatch. No production operation is authorized.

## Progress

- Implementation has not started. No build, daemon, GitHub, registry, deployment, or production Graphify action has occurred.