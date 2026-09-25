# docker-watcher Graphify Refresh — Progress Ledger

Status: **Requirements and design confirmed; tasks checkpoint pending; implementation gate closed.** No implementation dispatch is authorized until tasks confirmation and `.superpowers/sdd/GATE_OPEN`.

## SpecDD artifacts

- Requirements: `requirements.md` — original version confirmed (“continue”, 2026-10-05); revised FR-9/FR-10 separately confirmed in this conversation.
- Design: `design.md` — Critic-approved and confirmed by user in this conversation.
- Tasks: `tasks.md` — Critic-approved; separate user confirmation pending.

## Critic reviews

- First-draft review: Critic returned **REVISE** for requirements, design, and tasks. Findings: async ambiguity could violate no-overlap; post-build persistence leaves a crash gap; contract branch discovery may invalidate later checkpoints; unavailable live smoke must not count as integration acceptance; workspace mapping and nonterminal/poll-error handling need explicit treatment.
- Amendments applied: narrowed crash guarantee and recorded window; gated unresolvable async resubmission for a later user decision; moved read-only contract discovery after requirements but before final design/tasks checkpoints, with re-review/reconfirmation on material changes; separated local verification from end-to-end acceptance; added mapping, polling, and nonterminal behavior.
- Second review: Critic returned **REVISE** because an unresolved async attempt did not explicitly block a later generation's immediate submission or replay after restart/deadline.
- Additional amendments applied: unresolved potentially accepted work now blocks all generations per repo, later builds coalesce durably into its latest target, and elapsed eligibility never authorizes replay; added restart and A→B acceptance scenarios.
- Third review: Critic returned **REVISE** because accepted-only async acknowledgments could allow later overlapping requests without explicit approval, and the timeout-A → build-B test scenario was underspecified.
- Additional amendments applied: accepted-only semantics now block later submissions unless the user explicitly approves the overlap tradeoff; contract discovery must obtain that decision; added the exact timeout-A/coalesced-B/no-invocation acceptance scenario.
- Fourth review: Critic returned **REVISE** because the accepted-only task acceptance text contradicted the offered safe blocked option.
- Amendment applied: the accepted-only checkpoint accepts either retaining the blocked unresolved state or explicitly approving possible overlap.
- Final re-review of amended first draft: **APPROVE**; no further amendment required. Critic confirms the requirements checkpoint is ready.
- Material redesign reviews 1–3 returned **REVISE** and were amended. Review 4 returned **APPROVE** for the revised requirements/design/tasks user checkpoints, not implementation; its minor Docker exec-ID terminology amendment is applied. Critic approved explicit global workspace blocking, per-repo retry clocks, post-snapshot failure accounting, follow-on eligibility, watcher-probe evidence scope, and rollback controls. Requirements and design are separately confirmed; tasks confirmation remains pending.

## Deployed contract discovery

- Portainer inspection: `graphify-mcp` container `800c81850c30531cd6fa23578ea9ae2dbcfe75dcc32984db4d6713479128f7ab`, Graphify `0.9.67`, `/srv/mcp-grafana/graphify-out` → `/data`, `/scratch/workspace` → `/workspace`. `/data/graph.json` was 39,246,926 bytes; the manifest contains `docker-watcher/...` paths. No `/workspace/docker-watcher/graphify-out/graph.json` exists.
- `/workspace/docker-watcher` exists at commit `3506148fb54236c0e19551055d300a10cc34cae4`. The graph source identity includes `docker-watcher/app.py`; the graph is aggregate, rooted at workspace-relative paths. The checked-out revision is a snapshot, not proof it tracks each new push.
- The installed Graphify CLI is `0.9.67`. Source inspection showed `graphify update <path>` re-extracts code without an LLM and uses `_rebuild_code(..., block_on_lock=True)` on `out/.rebuild.lock`; `GRAPHIFY_OUT=/data` makes that `/data/.rebuild.lock`. The design enforces one watcher-owned workspace exec and treats Graphify’s lock only as defense, not as the watcher serialization guarantee. The update command itself has not run.
- Watcher-route probe: through Portainer-assisted `exec` into the running `docker-watcher`, the command `docker exec -e GRAPHIFY_OUT=/data graphify-mcp graphify check-update /workspace/docker-watcher` used the watcher’s configured `DOCKER_HOST` and exited 0. This proves the Docker CLI command path only; the proposed direct HTTP transport and production `graphify update` remain unverified.
- Docker Engine `/version` via Portainer reported `29.8.1`. Harmless Engine exec probes via Portainer observed: created/unstarted `Running=false, ExitCode=null`; running `sleep` `Running=true, ExitCode=null`; completed `true` `Running=false, ExitCode=0`; repeated start after completion returned HTTP 409 `already run`. These probes did not execute Graphify update or modify `/data/graph.json`.
- The proposed live update remains unverified. Before its first run, tasks require a frozen workspace, exclusive writer coordination, a durable archive backup, graph parse/readback checks, unaffected-repo node/edge comparison, and rollback on failure.
- Existing separate-workspace race is retained: indexing can see content newer than the pushed revision. No snapshot, clone transfer, or pinning was added.

## User checkpoints

- Requirements: original version confirmed (“continue”, 2026-10-05); revised FR-9/FR-10 separately confirmed in this conversation.
- Design: separately confirmed in this conversation after Critic APPROVE.
- Tasks: Critic-approved; separate user confirmation pending.

## Implementation

Blocked. No implementation agent has been dispatched and no Graphify behavior has been changed.
