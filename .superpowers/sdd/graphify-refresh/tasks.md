# docker-watcher Graphify Refresh — Tasks

Status: **requirements and design confirmed; tasks checkpoint pending.** Deployment discovery is complete. Read `requirements.md` and `design.md` first. Any material change requires fresh Critic review and separate user confirmation. Implementation remains blocked until task confirmation, all Critic verdicts, and the project gate are recorded.

## 1. Record verified deployed contract (complete; tasks checkpoint pending)

- Record Graphify `0.9.67`, its read-only MCP tools, the installed code-only `graphify update <path>` CLI, `GRAPHIFY_OUT=/data`, the aggregate `/workspace` source root, and the shared `/data/graph.json` in `design.md`.
- Record that Graphify HTTP is unreachable from the watcher network. A read-only `graphify check-update` executed inside the running watcher via Portainer-assisted exec and its configured `DOCKER_HOST` exited 0; this is CLI-path evidence, not proof of the proposed standard-library client or production update.
- Propose one serialized Docker Engine exec lifecycle (`create` → persist ID → `start` → `inspect`) updating `/workspace` into `/data`; keep unresolved executions blocked, and document code-only scope and the accepted push-to-build workspace race.
- The actual production update has not been run. Do not mutate the graph before tasks confirmation.

**Acceptance:** Design identifies the verified source/output mapping, invocation route, success and ambiguity semantics, and shared-graph serialization. Deployment evidence is recorded; task confirmation remains pending.


## 2. Persist per-repository refresh targets and shared execution state

- Keep successful Docker build completion observable separately from tag pushes. Add a durable per-repository target generation immediately after build success and before later pushes; preserve it if a push fails.
- Track per-repo target/completed generation, failure count, retry eligibility, and safe outcome plus one workspace-wide active exec record with its target-generation snapshot, attempt phase, `start_requested` marker, crash-recovery fallback deadline, exec ID, and Graphify container ID. No other repo may submit while this scan of the entire workspace or an unresolved result exists; new generations are durably coalesced. Per-repo retry clocks remain distinct even though one successful workspace update may satisfy multiple targets.
- Preserve old state compatibility and atomic persistence. Do not authorize any external action until its enabling state has been durably written.

**Acceptance:** Each completed state commit records a successful build generation even if a later push fails; failed/skipped/unchanged builds do not. Refresh failures cannot prevent remaining pushes or another repo. Persistence failure suppresses dependent Docker API actions. Crash before the post-build state commit may lose the trigger as documented.

## 3. Implement one serialized workspace exec with per-repo target tracking

- Invoke immediately after successful build only when that repo has no pending retry and no active/unresolved workspace exec exists. Use fixed argv `["graphify", "update", "/workspace"]`, `GRAPHIFY_OUT=/data`, unattached stdin/stdout/stderr, and a detached start with `Tty=false`; never build command text from repository data. One invocation scans the full workspace and counts as in-flight for all repos.
- Use the standard-library Docker Engine client over configured `DOCKER_HOST`; negotiate API version with `/version` and resolve exact `graphify-mcp` container before create. Support both current TCP endpoint and documented Unix-socket fallback.
- Persist attempt state before exec create. Persist returned exec ID before start and `start_requested` before the start request. A lost create response can only leave an unstarted orphan; apply backoff and retry safely.
- Preserve nullable Docker `ExitCode`: `Running=true/null` is active, `false/null` is not started, and `false/integer` is terminal. On ambiguous start, inspect the saved ID; retry the same ID only when still unstarted and only after persisted backoff; reconcile 409 `already run` by inspecting, never by creating a replacement.
- Inspect at most once per normal watcher cycle. Poll errors do not submit another exec. A running or uninspectable execution blocks all repos; elapsed time never authorizes replay or overlap.
- Builds from any repo during an exec durably advance targets and coalesce. Terminal success acknowledges only the saved snapshot; preserve retry count/deadline for newer generations not included in it. Start a follow-on only if some newer dirty target is eligible, otherwise wait until the earliest persisted due time. An eligible target’s full-workspace snapshot may include other not-yet-due targets, which success may satisfy. On nonzero exit, update retry clocks for every dirty target at failure observation, including generations added after the snapshot, because Graphify provides only a workspace-level exit code; each repo uses its own 60s/doubling/30m cap. Coalescing alone never resets a deadline. Persist a distinct attempt-start fallback deadline before create for unobserved crashes.

**Acceptance:** Deterministic clock/client scenarios prove immediate initial attempt, per-repo observed-failure timing versus attempt-start fallback, cross-repo coalescing behind one shared exec, unresolved workspace attempt blocking all repos, one inspect per cycle, no replay/overlap after ambiguous start, pre-start/active/terminal ExecInspect recovery, failed-start backoff persistence, terminal transitions, and persistence-failure suppression. Include crash-before-create, crash-after-create, crash-after-start, definite start rejection followed by restart before its due time, start timeout with inspect-by-ID, repo B arriving during repo A’s active generation followed by nonzero exit (B is failed too), B with a not-due retry gaining a generation after A’s snapshot (A success preserves B’s due time), B-only future-due follow-on wait, and eligible A opportunistically satisfying B.

## 4. Add safe events and operational documentation

- Emit structured `graphify_refresh` outcomes using only fixed internal values and validated repo/exec IDs. Never log Graphify stdout/stderr, credentials, raw Docker responses, exception text, or unvalidated host/path/status data.
- Keep Graphify errors separate from the existing literal `build failed` alert path.
- Document the verified Docker Engine exec route, shared workspace graph and lock, code-only refresh scope, nullable ExecInspect recovery matrix, retry behavior, unresolved-container-replacement limitation, and accepted workspace revision race. Add the bounded Docker API request-timeout setting to `.env.template`; do not add Graphify auth, network changes, mounts, or dependencies unless implementation evidence requires them.

**Acceptance:** Tests prove invalid/unsafe IDs and raw response/error content cannot appear in events. Retry timing is logged only after successful persistence. No secret or unsafe deployment value enters configuration or logs.

## 5. Verify application and live integration

- Extend `test_app.py` with consumer-visible checks for build-success target persistence, push-failure retention, no trigger for skipped/failed/unchanged builds, first refresh failure not stopping pushes, one workspace exec across repo triggers, generation snapshots/coalescing, global unresolved blocking, backoff/restart, nullable ExecInspect transitions, create/start ambiguity, poll bounds, terminal results, persistence failures, and safe events.
- Run targeted tests plus throwaway fake Docker Engine lifecycle and scheduler scenarios. Do not contact homelab services from unit tests.
- After implementation and approval, establish an exclusive maintenance window: freeze `/workspace`, stop/suspend refresh submissions, drain/inspect the active exec, and exclude other Graphify writers. Check free space; archive all `/data` graph/update files under timestamped `/data/.graphify-refresh-backups/` excluding older backups, then verify archive readability/checksum. Run the fixed update, inspect its saved exec ID, verify `graph.json` and `manifest.json` parse, compare unaffected-repo nodes/edges to the frozen pre-update baseline, and check the updated graph through Graphify MCP. On failure restore the archive, revalidate parsing/coverage/readback, then resume writers.

**Acceptance:** Local tests pass. Live acceptance covers the deployed end-to-end path, confirms unrelated aggregate graph nodes/edges are preserved against a frozen baseline, uses a verified rollback archive, and exposes no Graphify secrets in logs. If source freeze, exclusive writer coordination, backup, or safe smoke execution is unavailable, record the exact prerequisite and leave end-to-end acceptance and implementation sign-off open.
