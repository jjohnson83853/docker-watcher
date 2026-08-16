# docker-watcher — Requirements

Status: **signed off.** v1 exists and was built outside the architect pipeline. This
document is a retroactive requirements baseline covering (a) remediation of defects found
in design review, (b) retiring the per-repo self-hosted GitHub runner fleet in favour of
this single service, and (c) build-failure alerting.

## 1. Goal / problem statement

Every containerized project on the user's personal GitHub account needs its image built
and pushed to the LAN registry when its default branch moves. Today this is done by a
fleet of per-repo self-hosted GitHub Actions runners — one container, one registration
token and one `.github/workflows/build-and-push.yml` per repo. That fleet covers 5 of
~17 containerized projects and each new repo costs a manual runner registration.

`docker-watcher` replaces the fleet with one always-on container that discovers every
owned repo with a Dockerfile, watches the configured branch, and builds and pushes on
new commits. One container, one credential, zero per-repo setup.

## 2. Functional requirements

### 2.1 Discovery and watching
- FR-1 Enumerate all repos owned by `GITHUB_USERNAME` via the GitHub REST API, paginated.
- FR-2 For each owned repo, determine whether a `Dockerfile` exists at the repository
  root on the configured `BRANCH`. Repos without one are skipped.
- FR-3 Track the last successfully-built commit SHA per repo and rebuild only when the
  branch tip differs.
- FR-4 Poll on a fixed interval (`POLL_INTERVAL_SECONDS`, default 300).
- FR-5 Skip the per-repo Dockerfile and commit lookups entirely when the repo's
  `pushed_at` timestamp is unchanged since the last cycle, to hold steady-state API
  usage near zero.

### 2.2 Build and publish
- FR-6 On a detected change, shallow-clone the configured branch, build the root
  Dockerfile, and push to `REGISTRY_URL`.
- FR-7 Image name is the repo name with the `<username>/` prefix stripped
  (`jjohnson83853/playlist-sync` → `playlist-sync`).
- FR-8 Tag each build both `:latest` and `:<short-sha>`.
- FR-9 The SHA used for tagging and for state must be the commit actually checked out
  (`git rev-parse HEAD` post-clone), not the SHA observed before cloning.
- FR-10 Record state only after a push succeeds, so failures retry on the next cycle.

### 2.3 Migration
- FR-11 The watcher must build the 5 repos currently served by dedicated runners
  (`playlist-sync`, `swing-trade-mcp`, `schwab-mcp`, `mcp-market`, `mcp-second-brain`)
  with no per-repo configuration.
- FR-12 The per-repo runner fleet must be fully retired: workflow files removed,
  runner containers torn down, runner registrations revoked. No coexistence mode and
  no skip-list — see Non-goals.
- FR-13 `docker-watcher` builds itself. Bootstrap (first image) is a manual user build
  per the standing PR-based delivery model.

### 2.4 Alerting (in scope)
- FR-14 A build failure must produce a notification without the user having to read logs.
- FR-15 The application must emit a **stable, literal log prefix** on build failure that
  the alert rule matches. This prefix is an interface contract between `app.py` and the
  alert rule — changing it silently breaks alerting.
- FR-16 The alert must reach whatever notification channel the user's existing Grafana
  alerting already uses. No new notification channel, and no new service or component,
  is to be introduced for this.

## 3. Non-functional requirements

### 3.1 Security
- NFR-1 The GitHub PAT must never reach a pushed image layer. The clone's `.git`
  directory (which contains the tokenized remote URL) must not be part of the build
  context.
- NFR-2 The PAT must never be written to container logs, including via subprocess
  failure messages that render argv. This is reinforced by FR-14: log lines are now
  forwarded to Loki and surfaced in alert notifications, widening the blast radius of
  any leak.
- NFR-3 The PAT requires read-only contents scope only. No write scope on any repo.
- NFR-4 All configuration via `.env`. No secrets or environment-specific values in the
  image or in source.

### 3.2 Reliability
- NFR-5 A failure affecting one repo must not stop the cycle or the process.
- NFR-6 No operation may block indefinitely. Every subprocess gets a timeout.
- NFR-7 State writes must be atomic; a crash mid-write must not corrupt or truncate
  state (which would trigger a full rebuild of every repo).
- NFR-8 A misconfiguration that yields zero work (bad token, wrong username) must be
  visibly logged, not silently indistinguishable from "nothing to do".

### 3.3 Operational simplicity
- NFR-9 Python stdlib only; no third-party runtime dependencies.
- NFR-10 Single replica. No coordination, locking or clustering.
- NFR-11 Stay within GitHub's 5000 req/hr PAT budget with large headroom.
- NFR-12 Deployment is a `docker-compose.yml` + `.env.template` on `docker-lxc`.
- NFR-13 Alerting must add no new running component — it rides the existing
  `vector-agent` → Loki → Grafana pipeline (see `design.md` §1.1 EF-11).

## 4. Non-goals / out of scope

- NG-1 **Test gating.** The watcher builds on commit; it does not run repo test suites
  first. This is a deliberate, accepted downgrade from the runner fleet — see §7.
- NG-2 **Coexistence with per-repo runners.** Explicitly rejected by the user. No
  `build-and-push.yml` detection, no skip-list, no opt-out mechanism.
- NG-3 **Webhooks / any inbound ingress.** Polling only.
- NG-4 **Registry authentication.** The registry is open on the LAN (verified). No
  `docker login`, no registry credentials.
- NG-5 **Multi-branch, tag, or PR builds.** One configured branch per deployment.
- NG-6 **Non-root Dockerfiles, custom build contexts, build args, monorepos.** Root
  `Dockerfile`, root context, no args.
- NG-7 **Org repos / forks / collaborator repos.** Owned repos only.
- NG-8 **Deployment of built images.** Watchtower already handles redeploy.
- NG-9 **Liveness/heartbeat alerting.** FR-14 covers *build failures*. It does not cover
  the watcher dying or silently stopping, which produces no log line to match. Scoped out
  deliberately to keep this minimal; upgrade path noted in `design.md` §6.
- NG-10 **New notification channels, escalation, on-call routing, dashboards.** This is a
  homelab, not a paged on-call system.

## 5. Constraints

- C-1 Build host is `docker-lxc` (192.168.50.61); Docker API reached via the
  `socket-proxy` TCP endpoint, not a socket mount. Verified working in production.
- C-2 Registry is `registry.localdomain:5000`. Port 5000 mandatory.
- C-3 GitHub is source hosting only; no GitHub-hosted runners, ghcr.io or Docker Hub.
- C-4 GitHub account is **`jjohnson83853`** (confirmed). Prior references to
  `bullprog3` in `test_app.py` are wrong and must be corrected.
- C-5 Delivery is PR-based. The user reviews, merges and performs the bootstrap build
  by hand; agents do not build or push.
- C-6 **Resolved.** The CLAUDE.md standing default that would have recreated the retired
  fleet on every new repo has been amended by the user: docker-watcher now covers new
  Dockerfile repos automatically, with a per-repo workflow only where the user explicitly
  wants test-gating for that repo. FR-12 is therefore durable. This also gives NG-1 a
  documented escape hatch: a repo that genuinely needs pre-ship tests can still opt in to
  its own runner deliberately.
- C-7 Grafana is at `192.168.50.72:3000` (v13.1.3) and Loki at `192.168.50.72:3100`;
  its alerting API requires authentication, so the alert rule must be applied by the user
  or by `iac-devops` with credentials — not designed around anonymous access.

## 6. Open questions

None. All three prior open questions are resolved:

- ~~OQ-1~~ CLAUDE.md standing default — amended by the user. See C-6.
- ~~OQ-2~~ Watchtower exclusion — confirmed as proposed. See `design.md` ADR-9.
- ~~OQ-3~~ Failure alerting — **not deferred**; brought into scope as FR-14 to FR-16.

## 7. Accepted tradeoff, recorded explicitly

Retiring the runner fleet loses three things that the runners provided and the watcher
does not replace: build logs in the GitHub UI, PR-triggered builds, and the ability to
run tests before an image ships. The user has accepted this in exchange for one
container and one credential instead of N of each. Per C-6, individual repos can still
opt back in to a dedicated runner where test-gating genuinely matters.
