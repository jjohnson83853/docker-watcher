# Graphify Refresh Observability — Design

Status: **Critic-approved; requirements, design, and tasks separately confirmed by the user on 2026-09-30; dedicated implementation gate open.** Companion amendment to `.superpowers/sdd/graphify-refresh/design.md`.

## 1. Runtime boundary

`graphify_refresh.py` in docker-watcher calls Docker Engine `exec_create` against the `graphify-mcp` container. Graphify’s `graphify update /workspace` process runs in `graphify-mcp`, not in docker-watcher. Existing exec creation sets `AttachStdout=false` and `AttachStderr=false`, and start sets `Detach=true`; consequently the watcher never receives CLI output and Loki cannot ingest it from docker-watcher logs.

The selected design receives the non-TTY Docker exec stream in docker-watcher, incrementally classifies only approved fixed signatures, discards all stream bytes, and adds one enum-valued field to the existing `exec_failed` structured log event. The implementation MUST NOT log or persist raw output.

## 2. Approved classification vocabulary

Graphify `v0.9.67` source supports these exact bounded signatures:

- `workspace_path_missing`: the stderr signature `error: path not found:` occurs at the beginning of a line. The dynamic path suffix is never retained or emitted.
- `graphify_rebuild_failed`: the exact stderr line `Nothing to update or rebuild failed — check output above.` occurs.
- `unclassified`: a complete, well-formed stream matches neither approved signature.
- `unavailable`: the capture is incomplete, malformed, truncated, timed out, errored, cancelled, absent after restart, or not reported when a terminal failure is observed.

The first two names identify observed output signatures only; they do not assert a verified root cause, Graphify version, or authenticity of the line. Match stderr only. Use the fixed enum `workspace_path_missing` | `graphify_rebuild_failed` | `unclassified` | `unavailable`; a complete unmatched stream yields `unclassified`, while an incomplete stream yields `unavailable`. A byte-identical forged stderr line is indistinguishable and remains only a signature observation.

Source references: Graphify `v0.9.67` `graphify/cli.py`, `update` branch around lines 2420–2447; `.superpowers/sdd/graphify-refresh-observability/research.md` summarizes the reviewed output strings. Runtime version is not established by this contract; therefore the values are signature labels, never version-qualified cause claims.
- If both signatures appear in a complete stream, `workspace_path_missing` takes precedence; otherwise `graphify_rebuild_failed` precedes `unclassified`. Any incomplete or scan-capped stream is `unavailable`, even if a signature was observed before the cap.



## 3. Stream and lifecycle handling

- Create the exec with stdout and stderr attached and `Tty=false`; start it attached (`Detach=false`) so the Engine returns the multiplexed stream. This amends the detached/unattached lifecycle contract in `.superpowers/sdd/graphify-refresh/design.md` §§1 and 4, which MUST be amended before implementation.
- **Ownership and ordering:** The main loop creates the exec, persists its ID and `start_requested`, and retains the already-persisted global attempt slot before any start. It then hands the attempt to one dedicated worker. Only that worker calls attached `exec_start` and owns the stream connection; it never mutates persisted state, retry/backoff, or the global slot. The main loop never blocks on start, stream reads, or worker completion and continues build/push processing.
- The worker publishes a nonblocking, thread-safe start status (`pending`, `started`, `rejected` with sanitized HTTP status, or `transport_error`) when response headers/error become known, and publishes exactly one terminal capture status/classification when it exits. It always publishes a terminal record in `finally`; exceptions map to `unavailable`. The main loop observes both handoffs without waiting. It does not replace a worker while its attached start call is active; an eligible retry may launch one only after the prior worker has terminated and the existing persisted inspect/deadline rules permit it.
- The controller retains one in-memory active-worker reference. If terminal inspect clears the persisted attempt before the worker exits, no later Graphify exec is created until the worker has terminated; the normal build/push cycle continues. The worker cannot release the persisted slot, and the in-memory guard cannot authorize or change retry timing.
- **Bounded setup and cancellation:** Preserve the configured `DOCKER_HOST`, including `tcp://docker-lxc.localdomain:2375`; TCP hostnames must not be replaced or rejected. Before attached start, resolve hostnames in a bounded helper process; Unix sockets bypass resolution. One monotonic deadline begins before resolution and covers the resolver, all address connection attempts, request send, and complete response headers. Use only the remaining deadline for each operation and for header parsing so trickling bytes cannot extend setup indefinitely. The cancellation control registers the resolver process and socket before I/O; if already cancelled, no I/O begins; otherwise cancellation terminates the resolver child and closes the socket. Check cancellation before and after blocking operations. Setup timeout/cancellation exits through `finally`; the controller retains the worker reference until the thread terminates, then permits a later eligible exec. The main loop never waits.
- Only one potentially stalled resolver `Process.start()` helper may exist at a time. A timed-out startup retains that slot until its helper returns and terminates any late-started child, preventing retries from accumulating unbounded helper threads.
- Resolver startup completion, timeout, and abandonment are serialized; the late child is terminated and reaped before the startup slot is released, including completion-at-deadline races. Terminal cancellation only signals resolver termination without joining; the capture/startup worker owns reaping.
- Inspect the persisted exec ID on the normal cycle regardless of start acknowledgment or stream state. `ExecInspect` remains authoritative for running/terminal state and nullable `ExitCode`. A persisted/recovered ID with no live worker is never reattached while inspect reports running or terminal; if it is verified unstarted (`Running=false, ExitCode=null`), the existing same-ID recovery rule may start it after the applicable persisted deadline using a new worker.
- Keep the single-flight slot reserved from the persisted pre-create attempt through the existing terminal inspect/retry transition, including pre-ID and in-flight start. A worker never reserves or releases that slot. After restart, do not recreate a worker for a running or terminal saved exec; a later nonzero terminal result without a complete capture is `unavailable`.
- Parse Docker’s non-TTY 8-byte stream frames incrementally: stream type, three zero-reserved bytes, and big-endian payload length; accept only stdout/stderr frame types, reject declared frame payloads larger than `MAX_FRAME_BYTES = 1,048,576`, and scan stderr only. Preserve partial header/payload bytes across reads. On malformed framing or oversized frames, mark capture unavailable and continue draining/discarding in fixed chunks until EOF or terminal cancellation so the exec is not blocked by an unread stream.
- Success behavior and existing events remain unchanged; only terminal nonzero failures receive `diagnostic_class`. Never emit a diagnostic before `ExecInspect` confirms a nonzero terminal exit.
- At terminal nonzero inspect, select a completed capture result if already available, otherwise `unavailable`, then signal cancellation without waiting. Persist the retry transition before emitting exactly one `exec_failed` event with that classification. If persistence fails, retain the saved attempt and emit only `persist_failure`; on success, discard any late worker result and never emit a second event.
- At any terminal inspect (success or failure), signal cancellation if the capture worker is still active. On terminal success, discard its classification and preserve the existing success event/state behavior.

- **Worker termination:** HTTP `Content-Length` bodies are complete only after the declared byte count; chunked bodies require the terminating trailer section, including its final empty line. Premature close or malformed HTTP body framing gives `unavailable`. Close-delimited responses use socket EOF as their HTTP delimiter. Within the framed body, clean EOF is complete only at a Docker frame boundary. Partial header/payload at EOF, malformed frame, oversized frame, read error, or worker exception gives `unavailable`. A per-read socket timeout is nonterminal while the saved exec remains running: retry the read and check cancellation. The absolute setup deadline does not impose an overall Graphify-command timeout. On terminal inspect, the main loop signals cancellation and closes the worker-owned socket; on process exit, the OS closes open descriptors, but no orderly worker completion or classification is guaranteed. The worker publishes exactly one terminal record through `finally` when it exits.
- Validate HTTP field names as token syntax and field values as HTAB, SP, visible ASCII, or obs-text; reject other control bytes. Accept chunk-size tokens only as nonempty ASCII hexadecimal digits before any extension. Preserve trailer-line bytes and the cumulative trailer limit across idle read timeouts so framing can resume without discarding partial data.
- Validate the complete HTTP status line before reporting `started`: accept only HTTP/1.0 or HTTP/1.1, exactly three ASCII status digits, and a valid reason phrase. Malformed status lines yield `unavailable`.

## 4. Privacy and resource limits

- Use a fixed-size read buffer and named limits `MAX_FRAME_BYTES = 1,048,576` and `SCAN_CAP_BYTES = 65,536`. `SCAN_CAP_BYTES` counts all stdout and stderr frame payload bytes combined; Docker frame headers do not count. If total payload is greater than the cap, stop decoding, continue draining/discarding with the same fixed buffer until EOF, terminal-inspect cancellation, or process shutdown, and classify the stream `unavailable` even if a signature appeared before truncation. If payload size is at or below the cap, a complete well-formed stream may be classified normally. Do not close a running exec stream merely because the scan cap was exceeded. Main-loop terminal cancellation closes the connection only after `ExecInspect` has confirmed terminal status. This bounds classifier memory/work while preserving the attached process lifecycle.
- Never retain raw output in durable state, exception messages, `repr`, thread names, debug logs, or test failure diagnostics. Events contain only the existing safe fields plus `diagnostic_class` from the fixed enum.
- Preserve existing repo/exec ID validators and event allowlist. Docker HTTP status and error type remain sanitized as today.
- No new dependency, environment variable, volume, network, container log setting, Loki label, or Graphify configuration.

## 5. Failure and recovery behavior

- Exec start rejection/transport failure continues through the current inspect/recovery behavior. Capture failure alone does not count as Graphify command failure, alter retry timing, or authorize another start.
- If `ExecInspect` reports running, keep the attempt pending regardless of capture completion. If `ExecInspect` reports a nonzero terminal code, record the existing retry transition and emit one failure event; use the capture result if already complete, otherwise `unavailable`.
- If a stream is malformed, oversized, lost, or incomplete, record only `unavailable`; no raw parser/socket error reaches logs.
- No diagnostic data is added to the persisted state schema, avoiding migration and rollback-state changes.

## 6. Verification

Deterministic tests will use fake Docker HTTP responses/streams and fake watcher cycles; they will not run Docker, Graphify, Loki, or production operations. They must prove:

- Docker frame parsing across split headers/payloads and multiple stdout/stderr frames.
- At least two distinguishable approved signatures and precedence, plus unknown, absent, malformed, oversized, truncated Docker frames, premature `Content-Length` EOF, incomplete chunked trailers, and restart-unavailable behavior.
- Raw output containing paths, credentials, control characters, JSON-looking text, or exception details never appears in captured logs or persisted state.
- The main watcher cycle does not wait for a pending capture worker; capture completion never changes ExecInspect terminal semantics, persistence ordering, retry timing, serialization, or build/push isolation.
- Existing lifecycle/recovery and secret-free event tests still pass.
