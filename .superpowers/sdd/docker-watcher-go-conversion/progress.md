# docker-watcher Go conversion — Progress Ledger

Status: **requirements, design, and tasks confirmed by user; scoped implementation authorization recorded; saved SpecDD critic review ACCEPT; conversion gate open.**

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

- Root `.superpowers/sdd/GATE_OPEN` now records the conversion gate opening after the four SpecDD artifacts were committed.
- Conversion SpecDD commit: `09f5a1f5185adcaacb456c662234e3d92daa1ed8` (`docs: open docker-watcher Go conversion gate`).
- TDD RED checkpoint: `027594c89d8216577d3de2ebd7e928da3528f1ac` (`test: preserve Go RED checkpoint`). `go test ./...` failed at compile time on absent production APIs including `Repository`, `ExecInspection`, and `State`; this is intended RED evidence before production code.
- Conversion implementation gate: **open for the approved offline Go conversion only**. Live Docker/Graphify, image build/push, deployment, registry writes, SSH, and alert provisioning remain unauthorized.

## Progress

- Phase 1–4 implementation and offline verification are complete. Full suite: `go test -coverprofile=coverage.out -timeout=60s ./...` passed; root package 90.4% statement coverage (cmd/watcher 80.0%). Race suite, `go vet ./...`, `go build ./...`, `gofmt -d *.go cmd/watcher/*.go`, and `docker compose config -q` passed. `go run ./cmd/watcher` with empty required credentials exited 1 with the expected actionable configuration error. No production Docker/GitHub/registry/Graphify endpoints were contacted; no image build/push or deployment occurred.
- Final specialist reviews: code reviewer approved the state path/durability changes with no findings; Go reviewer reported no actionable findings; security reviewer reported one accepted LOW risk: a pre-existing state file's permissions are not tightened during load (new/replaced files use `0600`). State volume and ancestry must remain operator-controlled/stable; path-based checks do not defend against hostile concurrent local filesystem mutation.
- Review-driven amendments include exact `json.Number` state preservation, atomic same-directory state replacement and sync barriers across lexical, intermediate symlink-parent, and final resolved ancestry, retry-safe fail-closed loading, absolute cleaned state path for all I/O and leaf-symlink rejection; typed safe Git exit/timeout classification; clone-only PAT cleanup; run-context Graphify cancellation and durable handling of an exec ID returned concurrently; sanitized Graphify persistence-failure events; 8 MiB GitHub/1 MiB Docker response bounds; and capped Graphify repository logs. Stream scan cap drains/discards to EOF or cancellation; no total Graphify command timeout.
- RED/GREEN evidence: cancellation tests exposed missing `Graphify.ctx` and lost successful `ExecCreate` IDs during cancellation; target-save event tests exposed missing `persist_failure` logs; GitHub tests exposed oversized response and page acceptance before bounds; state tests exposed skipped ancestry retry, relative/symlink path omissions, and I/O mismatch on symlink-plus-`..`; relative/multi-hop symlink tests exposed omitted intermediate parents and leaf-symlink acceptance. Focused regression tests passed after fixes. Full-suite coverage is above the required 90%.
- The Docker CLI package pin is `29.8.2-r0`; `.env.template` and design artifacts match. Port 2375 is documented as plaintext/unauthenticated and must stay on a trusted isolated network or protected tunnel. Full GitHub pagination remains uncapped to preserve complete enumeration; aggregate repository memory remains the security review's LOW resource-exhaustion risk. Phase 5 remains user-owned.