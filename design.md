# docker-watcher — Design

Status: **signed off.** Implements `requirements.md`. Requirement IDs (FR-n, NFR-n, C-n,
NG-n) are referenced, not restated.

## 1. Environment findings

### 1.1 Verified (live read-only queries against the homelab, 2026-08-16)

| # | Finding | How verified |
|---|---|---|
| EF-1 | Daemon is Docker 29.4.0, API 1.54, MinAPI 1.40, overlayfs, 32 CPU / 67 GB. | `GET /version`, `GET /info` |
| EF-2 | `socket-proxy` (tecnativa) ACLs permit builds: `BUILD=1 POST=1 SESSION=1 IMAGES=1 CONTAINERS=1 NETWORKS=1 VOLUMES=1`. `GRPC=0`, `DISTRIBUTION=0`, `AUTH=0`. | container inspect |
| EF-3 | HAProxy config is patched to `timeout client/server/tunnel 60m`. | container inspect `Cmd` |
| EF-4 | `POST /build` returns 500 and `POST /session` returns 400 for empty probe bodies — daemon-generated rejections, not proxy 403s. The proxy forwards build traffic. | direct probe |
| EF-5 | **Five self-hosted runners build through this same proxy today** via `DOCKER_HOST=tcp://docker-lxc.localdomain:2375`, no socket mount, unprivileged. All five repos have images in the registry. | container inspect, `GET /v2/_catalog` |
| EF-6 | `registry.localdomain:5000` is registered on the daemon with `Secure: false`; `/v2/` returns 200 with no auth challenge and `_catalog` is anonymously readable. Registry is open for read and write on the LAN. | `GET /info`, direct probe |
| EF-7 | Registry holds 17 repositories — the real scale of the fleet the watcher must cover. | `GET /v2/_catalog` |
| EF-8 | GitHub account is `jjohnson83853`. | runner `REPO_URL` env |
| EF-9 | `socket-proxy` lives on network `socket_net` (with `watchtower` and `portainer`) and publishes 2375 on the host. | container inspect |
| EF-10 | `socket-proxy` mounts `/var/run/docker.sock:ro`. The `:ro` flag does not restrict the API — EF-4 writes reached the daemon through it. | container inspect + EF-4 |
| EF-11 | **Loki label schema** (`192.168.50.72:3100`, ready): labels are `host`, `job`, `level`, `service_name`, `source_subsystem`, `source_system`. Docker containers on `docker-lxc` appear as `job="vector-logs"`, `host="192.168.50.61"`, `source_system="docker_proxmox"`, `source_subsystem="<container-name>"` (confirmed for `local-registry-mirror`, `go2rtc`). | `/loki/api/v1/labels`, `/loki/api/v1/series` |
| EF-12 | Grafana is v13.1.3 at `192.168.50.72:3000`. `/api/health` is anonymous; all alerting/provisioning endpoints return 401. | direct probe |
| EF-13 | `vector-agent` mounts the Docker socket read-only and is configured from `/srv/vector_agent/config/vector.yaml` on `docker-lxc`; it already ships all container stdout to Loki with no per-container configuration. | container inspect |

### 1.2 Correction to prior guidance

`CLAUDE.md` records that `docker build` through the socket-proxy "hangs indefinitely or
fails with 403". EF-2/EF-4/EF-5 show this is **not true for containers on the LAN**. The
recorded failures were from an *external agent sandbox* reaching across network hops; the
`~34.9s grpc: the client connection is closing` signature is an intermediate-hop idle
timeout. The separate buildx `403` is fully explained by `GRPC=0` (EF-2) — buildx's
`docker-container` driver needs the `/grpc` Upgrade endpoint. Neither affects this design.

Recommend the user narrow that CLAUDE.md note to "remote builds *from an external agent
sandbox* fail" so it stops deterring correct designs. User action, no code impact.

### 1.3 Assumptions (not verified)

- A-1 The PAT will be issued with read-only contents scope across all owned repos (NFR-3).
- A-2 Watched repos use root `Dockerfile` + root context, no build args (NG-6).
- A-3 Alpine's `docker-cli` package can reach a version compatible with API 1.40+ (EF-1
  MinAPI makes this near-certain for any modern release).
- A-4 `vector-agent` will pick up the new container automatically and label it
  `source_subsystem="docker-watcher"` following the EF-11 pattern. **This is the one
  assumption the alerting design rests on and it must be verified empirically before the
  alert rule is written** — see task D-9.
- A-5 Grafana's default notification policy is wired to a contact point the user actually
  monitors. Unverifiable from here (EF-12: 401). Verified in D-11.

## 2. Architecture decision record

### ADR-1 — Docker access via `tcp://docker-lxc.localdomain:2375`
**Decision.** Keep the socket-proxy endpoint. Use the host-published address
`docker-lxc.localdomain:2375`, **not** `socket-proxy:2375`.

**Rationale.** EF-5 is decisive: this exact endpoint builds five repos in production
today. Using the published host port rather than the `socket_net` service name means the
compose file needs no external-network plumbing, and it copies a proven-working value
verbatim. The v1 compose file declared no networks at all, so `socket-proxy:2375` would
have failed DNS resolution immediately — a deployment-blocking bug.

**Alternatives rejected.** Mounting `/var/run/docker.sock` (unnecessary; the proxy works).
A standalone `buildkitd` container (another service to run, solves a problem that does not
exist). A dedicated build host (same). Joining `socket_net` (equivalent, more config).

**Note on blast radius.** With `POST/BUILD/CONTAINERS/EXEC/IMAGES=1` the proxy is not a
meaningful security boundary for this container, and EF-10 confirms `:ro` adds nothing.
That is acceptable and inherent: a service whose job is building arbitrary Dockerfiles is
root-equivalent on the daemon by definition. The proxy's value here is uniformity with
the rest of the estate, not isolation. Recorded so nobody mistakes it for containment.

**Fallback.** If builds ever stall, `DOCKER_HOST=unix:///var/run/docker.sock` plus a bind
mount is a one-line `.env` change. The design is transport-agnostic; this risk is cheap
and reversible. The stale "KNOWN RISK" comment in `.env.template` is removed.

### ADR-2 — Build path selected by env var, not baked in
**Decision.** `DOCKER_BUILDKIT` is an `.env` knob, default `1`. Pin `docker-cli` to an
explicit version in the Dockerfile (C-1 stability, A-3).

**Rationale.** Daemon-side BuildKit uses `POST /build?version=2` + `POST /session` — both
allowed (EF-2) — and is the non-deprecated path. The classic builder (`0`) needs only
`POST /build`, the narrowest surface. Alpine's `docker-cli` ships without the buildx
plugin, so v1 was selecting a build path *by accident* depending on base-image drift.
Making it an env var means either path is testable without a rebuild, and pinning the CLI
stops a base-image bump from silently switching behaviour.

### ADR-3 — Central watcher fully replaces the per-repo runner fleet
**Decision.** Per FR-12/NG-2: full takeover. No detection, no skip-list, no coexistence.

**Rationale.** At 17 repos (EF-7) the fleet costs 17 containers, 17 registration tokens
and 17 workflow files; the watcher costs one container and one PAT. Coexistence on a
given repo means two builders racing on `:latest` with different tag schemes (the workflow
tags full 40-char `github.sha`, the watcher tags 7-char). The user chose takeover over the
skip-check I originally proposed. Migration sequencing in §5.

**Cost accepted.** Loss of test gating, PR builds and GitHub-UI logs — recorded in
`requirements.md` §7. Per C-6 a specific repo can still opt back in to its own runner
where pre-ship tests genuinely matter.

### ADR-4 — `.git` is removed from the build context before building
**Decision.** After clone: `git rev-parse HEAD`, then delete `<tmpdir>/.git`, then build.

**Rationale.** The highest-severity defect in v1. The clone URL embeds the PAT, so
`.git/config` contains it; the whole tmpdir is passed as build context, so any watched
repo whose Dockerfile does `COPY . .` bakes the PAT into a layer and pushes it to a
registry that is anonymously readable on the LAN (EF-6). Satisfies NFR-1. Ordering the
`rev-parse` first also supplies the true built SHA for FR-9, so one change closes two
defects. Side benefit: smaller, faster build context.

### ADR-5 — Token never rendered into logs
**Decision.** Catch subprocess failures and log `returncode` and the repo name only —
never the exception's default string form.

**Rationale.** `CalledProcessError.__str__` renders full argv, which includes the
tokenized clone URL. v1 logged exactly that. Satisfies NFR-2. This matters more now than
at review time: under ADR-11 these lines are shipped to Loki and pasted into alert
notifications, so a leak would propagate to the notification channel too.

### ADR-6 — Bounded subprocesses, atomic state, broad per-repo catch
**Decision.** Every `subprocess.run` gets an explicit `timeout`. `save_state` writes a
temp file then `os.replace`. The per-repo handler catches `Exception`, not just
`CalledProcessError`.

**Rationale.** Three distinct v1 failure modes with one theme — the process could get
stuck or lose data with no operator signal. Unbounded subprocess plus
`restart: unless-stopped` is the worst combination: a hung build leaves the process alive,
so nothing ever restarts it (NFR-6). Non-atomic `open(...,"w")` truncates state on a crash
mid-write, forcing a rebuild of all 17 repos (NFR-7). A `URLError` from the API helpers
escaped the `CalledProcessError`-only handler and aborted the whole cycle (NFR-5).

### ADR-7 — `pushed_at` prefilter; state schema carries it
**Decision.** State becomes `{repo: {"sha": ..., "pushed_at": ...}}`. Skip a repo entirely
when `pushed_at` is unchanged. Tolerate the v1 bare-string shape on read.

**Rationale.** v1 spent 2 API calls per repo per cycle regardless of activity: at 17 repos
and 300s that is ~420 calls/hr, and it scales linearly with repo count. The repo list
already returns `pushed_at` for free, so steady state drops to ~12 calls/hr (NFR-11).
`pushed_at` moves on any branch push, so it is a conservative prefilter — false positives
harmlessly fall through to the existing checks. Reading the old shape avoids a needless
17-repo rebuild on upgrade.

### ADR-8 — Misconfiguration must be loud
**Decision.** Log every non-200 GitHub API response with status and URL. Log the repo
count discovered each cycle.

**Rationale.** v1's `gh_get` collapsed `HTTPError` to `(status, None)` and
`list_owner_repos` broke the loop on any non-200 and returned `[]`. A 401 from an expired
PAT was therefore indistinguishable from "no repos" — the service would run forever,
silently, building nothing. Combined with the wrong username in v1 (`bullprog3` vs EF-8
`jjohnson83853`, C-4) this was a realistic path to a completely inert service with a clean
log. Satisfies NFR-8.

### ADR-9 — Watchtower does not manage the watcher itself
**Decision.** Label the service `com.centurylinklabs.watchtower.enable=false`. Watcher
upgrades are a manual `docker compose pull && up -d`. **Confirmed by the user.**

**Rationale.** The watcher builds itself (FR-13). Under Watchtower it would push its own
new image and then be killed by Watchtower mid-cycle, potentially mid-build of some other
repo. Self-healing (state is written only after a successful push, so the interrupted repo
rebuilds next cycle) but it leaves dangling builds and makes failures hard to read — and
under ADR-11 it would also emit alert-triggering failure lines for builds that were merely
interrupted. One label is cheaper than reasoning about the race. All *other* services keep
Watchtower.

### ADR-10 — Retained from v1 without change
Polling over webhooks (NG-3: webhooks need inbound ingress for a job with 5-minute latency
tolerance). Single JSON state file, no locking (NFR-10, single replica). No registry auth
(NG-4, justified by EF-6). Stdlib-only (NFR-9). State recorded only after a successful push
(FR-10). These were correct in v1 and are explicitly not being touched.

### ADR-11 — Alerting: one Grafana-managed Loki rule, routed to the default policy
**Decision.** Satisfy FR-14/15/16 with a **single Grafana-managed alert rule** querying the
existing Loki datasource for the literal string `build failed` in the watcher's own log
stream. No new component, no new contact point, no application changes beyond a stable log
prefix.

Query (grounded in EF-11's actual label schema, not a guess):

```logql
sum(count_over_time(
  {job="vector-logs", host="192.168.50.61", source_subsystem="docker-watcher"}
  |= "build failed" [10m]
))
```

Condition `> 0`, evaluated every 5m, `for: 0m` (fire on first evaluation — a single failed
build is worth one notification).

**Rationale, and why nothing more.** The pipeline already exists end to end: `vector-agent`
picks up every container's stdout with zero per-container config (EF-13) and Loki/Grafana
are already running (EF-11, EF-12). So the entire feature is *one rule*, which is exactly
the right size for "tell me when a build breaks" in a homelab. Prometheus metrics, a
`/metrics` endpoint, a pushgateway, a healthcheck sidecar or a dedicated notifier would all
be new moving parts to maintain for a signal that a text match already provides (NFR-13).

**Routing.** The rule deliberately declares **no custom contact point or notification
policy** — it falls through to Grafana's default notification policy, whichever contact
point that already points at. This satisfies FR-16 without needing to know what the sink
is (EF-12 blocks enumeration), and means the alert automatically matches however the user
already receives every other Grafana alert. If the user later changes their notification
channel, this rule follows with no edit.

**The coupling this creates.** The literal `build failed` prefix is now a load-bearing
interface (FR-15), not just a log message. It is specified in E-4 and asserted by a test in
E-9 precisely so that a future reword of the log line fails a test rather than silently
disabling alerting months later. This is the one genuinely fragile part of the design and
it is cheap to defend, so it is defended.

**Alternatives rejected.** Alerting on the `level="error"` label (EF-11) — vector infers
level heuristically from plain stdout, so it is less predictable than an exact string we
control. A `docker-watcher`-specific dashboard — nothing to look at between failures.

## 3. Threat model and secret handling

| Threat | Vector | Mitigation |
|---|---|---|
| PAT exfiltrated via published image | `.git/config` in build context + `COPY . .` | ADR-4 |
| PAT in container logs, now also forwarded to Loki and into alert notifications | `CalledProcessError` argv rendering | ADR-5, reinforced by NFR-2 |
| PAT in process table | token in `git clone` argv | Accepted. Single-purpose container, no other processes, no untrusted local users. Noted as the residual. |
| PAT over-scoped | classic PAT with `repo` write | NFR-3: read-only contents scope. User-issued. |
| Malicious/compromised Dockerfile | `RUN` executes as root on the build daemon | **Not mitigated, inherent.** Source repos are the user's own private repos; trust boundary is the GitHub account itself. Same exposure the runner fleet already had. |
| Registry tampering | EF-6: registry is anonymously writable on the LAN, and Watchtower auto-deploys from it | **Pre-existing environment risk, out of scope.** Flagged to the user; not introduced or worsened by this design. |
| Secrets at rest | `.env` on `docker-lxc` | `.gitignore` excludes `.env`; only `.env.template` is committed (already correct in v1). |
| Grafana service-account token | held by `mcp-grafana`; alert rule applied with user credentials | Not handled by this service. C-7: the rule is applied by the user or `iac-devops`, and no Grafana credential is added to this repo or its `.env`. |

Retiring the fleet is a net security win: it removes 5 separate runner `ACCESS_TOKEN`
copies from the environment, leaving one PAT.

## 4. Repository and deployment structure

```
docker-watcher/
  app.py                        # single module, stdlib only
  test_app.py                   # pure-function checks, no network
  Dockerfile                    # python:3.12-alpine + pinned docker-cli + git
  docker-compose.yml            # example deployment
  .env.template                 # all config, no secrets
  alerts/build-failed.yaml      # Grafana alert rule (ADR-11), version-controlled
  .gitignore
  requirements.md design.md tasks.md
  .github/PULL_REQUEST_TEMPLATE.md
```

`.github/workflows/build-and-push.yml` is **deleted** — it is exactly the per-repo pattern
ADR-3 retires, no runner is registered for this repo, and leaving it would have the watcher
and a workflow both building this image.

`alerts/build-failed.yaml` lives in this repo rather than only in Grafana's UI so the rule
is reviewable in the PR and survives a Grafana rebuild. It is applied to Grafana out of
band (C-7); nothing in the container reads it.

Deployment: `docker compose up -d` on `docker-lxc`. Persistent service, one replica, named
volume for `/data`. Bootstrap image built by hand (C-5); thereafter the watcher builds
itself, deployed manually per ADR-9.

`.env.template` keys: `GITHUB_TOKEN`, `GITHUB_USERNAME` (default `jjohnson83853`),
`BRANCH`, `REGISTRY_URL`, `DOCKER_HOST` (`tcp://docker-lxc.localdomain:2375`),
`DOCKER_BUILDKIT`, `POLL_INTERVAL_SECONDS`, `STATE_FILE`, `BUILD_TIMEOUT_SECONDS`.
Do **not** copy the runners' `DOCKER_TLS_VERIFY=tcp://...` — that is a cargo-culted bug in
their config; a non-empty value means "verify TLS", wrong for a plaintext endpoint.

## 5. Migration: retiring the runner fleet

Ordering matters — the fleet must not be dismantled before the watcher is proven, or those
five repos stop being built.

1. **Deploy and prove.** Watcher runs alongside the untouched fleet. Confirm it builds all
   ~17 Dockerfile repos, including the five runner-managed ones.
   *A short parallel period is safe:* both builders produce the same content from the same
   commit, so the only cost is duplicated work and `:latest` tag churn, not divergence.
2. **Remove workflows.** One PR per runner-managed repo deleting
   `.github/workflows/build-and-push.yml`. Do this *before* stopping runners.
3. **Tear down runners.** Stop and remove the five `runner-repo-*` containers.
4. **Revoke registrations.** Delete each runner in GitHub repo settings, and revoke the
   five `ACCESS_TOKEN` PATs.

**Why not leave them inert.** Stopping runners while leaving workflow files in place is the
worst option: every push queues a job with no runner to take it, so each repo accumulates
jobs that hang for ~24h before failing, and every commit shows a red/pending check forever.
Deleting the workflow file is the clean stop. Hence the step-2-before-step-3 ordering.

**Regrowth risk: closed.** Per C-6 the user has amended the CLAUDE.md standing default, so
new Dockerfile repos no longer get a per-repo workflow by default. This migration is now
durable.

## 6. Storage, backup, monitoring, alerting

**Storage.** One named volume holding `state.json` — a few KB of last-built SHAs.

**Backup.** None needed, deliberately. State is derived, not authoritative: losing it
causes one rebuild of every repo, which is self-healing and costs only build time. Backing
up a cache would be pure ceremony. ADR-6's atomic write is what actually matters here.

**Monitoring.** stdout only, captured by the existing `vector-agent` → Loki pipeline with
no per-container configuration (EF-13). Log lines must be greppable and must never contain
the token (ADR-5): cycle start with repo count (ADR-8), per-build start with repo + SHA +
target image, per-build outcome, every non-200 API response.

**Alerting.** Per ADR-11: one Grafana-managed rule matching the literal `build failed`
prefix in this container's Loki stream, routed to the default notification policy. Delivered
as part of this build (FR-14), not deferred.

**Known gap, deliberately not covered (NG-9).** This alerts on builds that fail, not on the
watcher dying, hanging, or never starting — those produce no matching line, and a
log-absence rule is a different shape of check. The upgrade, if the user ever wants it, is
one more rule in the same file using `absent_over_time` on the same stream selector over a
window a few multiples of `POLL_INTERVAL_SECONDS`. Not built now because it needs tuning to
avoid false alarms on restarts, and the failure it catches is rarer than the one we do
catch. Named here so it is a known gap rather than an assumed capability.
