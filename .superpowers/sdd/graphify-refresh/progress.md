# docker-watcher Graphify Refresh — Progress Ledger

Status: **SpecDD draft; implementation gate closed.** The presence of this workspace blocks implementation dispatch until the verdicts and three separate user confirmations below are recorded and `.superpowers/sdd/GATE_OPEN` is written.

## SpecDD artifacts

- Requirements: `requirements.md` — draft; user confirmation pending.
- Design: `design.md` — draft; user confirmation pending.
- Tasks: `tasks.md` — draft; user confirmation pending.

## Critic reviews

- First-draft review: Critic returned **REVISE** for requirements, design, and tasks. Findings: async ambiguity could violate no-overlap; post-build persistence leaves a crash gap; contract branch discovery may invalidate later checkpoints; unavailable live smoke must not count as integration acceptance; workspace mapping and nonterminal/poll-error handling need explicit treatment.
- Amendments applied: narrowed crash guarantee and recorded window; gated unresolvable async resubmission for a later user decision; moved read-only contract discovery after requirements but before final design/tasks checkpoints, with re-review/reconfirmation on material changes; separated local verification from end-to-end acceptance; added mapping, polling, and nonterminal behavior.
- Second review: Critic returned **REVISE** because an unresolved async attempt did not explicitly block a later generation's immediate submission or replay after restart/deadline.
- Additional amendments applied: unresolved potentially accepted work now blocks all generations per repo, later builds coalesce durably into its latest target, and elapsed eligibility never authorizes replay; added restart and A→B acceptance scenarios.
- Third review: Critic returned **REVISE** because accepted-only async acknowledgments could allow later overlapping requests without explicit approval, and the timeout-A → build-B test scenario was underspecified.
- Additional amendments applied: accepted-only semantics now block later submissions unless the user explicitly approves the overlap tradeoff; contract discovery must obtain that decision; added the exact timeout-A/coalesced-B/no-invocation acceptance scenario.
- Re-review of amended draft: pending.

## User checkpoints

- Requirements: pending.
- Design: pending.
- Tasks: pending.

## Implementation

Blocked. No implementation agent has been dispatched and no Graphify behavior has been changed.
