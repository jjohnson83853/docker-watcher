# docker-watcher — Tasks

Status: **signed off — ready for `engineer`.** Execution plan for `design.md`. Decisions
are referenced by ADR number, not re-explained. Read `requirements.md` and `design.md` first.

Delivery is PR-based (C-5): finish and test, open a PR with the artifacts, the user
reviews, merges and performs the bootstrap build by hand. No agent builds or pushes.

---

## Section A — Engineer (application code)

### E-1 Correct the GitHub username
Replace `bullprog3` with `jjohnson83853` in `test_app.py` (C-4, EF-8). Grep the whole repo
for `bullprog3` to catch any other reference.

**Acceptance:** `grep -ri bullprog3 .` returns nothing.

### E-2 Log every non-200 GitHub API response (ADR-8)
`gh_get` must log status + URL on any non-200 before returning. `list_owner_repos` must
log the discovered repo count each cycle, including zero.

**Acceptance:** With a deliberately invalid `GITHUB_TOKEN`, the log shows an explicit 401
against the repos URL and a `0 repos` line — not silence. This is the check that a
misconfigured deployment is diagnosable.

### E-3 Broaden the per-repo exception handler (ADR-6)
`run_once`'s per-repo `except subprocess.CalledProcessError` becomes `except Exception`,
logging repo name and error type. A failure on one repo must not end the cycle.

**Acceptance:** A unit check where one repo's API call raises `urllib.error.URLError`
confirms subsequent repos are still processed.

### E-4 Stop leaking the PAT into logs, and fix the failure log line (ADR-5, ADR-11, NFR-2)
No log line may render subprocess argv. Log repo name and `e.returncode` only. Applies to
the handler broadened in E-3 — a bare `except Exception` logging `{e}` reintroduces the
leak, so E-3 and E-4 must be written together.

The failure line must begin with the **exact literal string `build failed`** (FR-15). This
is an interface contract with the alert rule in D-10, not a cosmetic choice — see ADR-11.
Suggested shape: `build failed for <repo>: exit <returncode>`. Add a comment at that line
stating that `alerts/build-failed.yaml` matches on this prefix.

**Acceptance:** Grep the diff — no f-string interpolates a `CalledProcessError` object or
any variable derived from the tokenized clone URL. A forced clone failure produces a log
line starting `build failed` with no token substring anywhere in it.

### E-5 Bound every subprocess (ADR-6, NFR-6)
Every `subprocess.run` in `build_and_push` gets an explicit `timeout=`, sourced from a new
`BUILD_TIMEOUT_SECONDS` env var (default 1800). `TimeoutExpired` is caught by E-3's
handler and must also log the `build failed` prefix per E-4 — a hung build is a failed
build and must alert.

**Acceptance:** With the timeout set to 1, a build attempt logs a `build failed` line and
the loop proceeds to the next repo. The process stays alive.

### E-6 Remove `.git` from the build context and use the real built SHA (ADR-4, FR-9)
In `build_and_push`, after the clone: run `git rev-parse HEAD` to get the checked-out SHA,
then delete `<tmpdir>/.git`, then build. Tags and state use the `rev-parse` SHA, not the
SHA passed in from the API. Highest-severity item in this task list (NFR-1).

**Acceptance:** Build a fixture repo whose Dockerfile does `COPY . .`; the resulting image
contains no `.git` directory and no token string in any layer. A second check asserts the
tag matches `git rev-parse HEAD` of the clone, not the pre-clone SHA.

### E-7 Atomic state writes (ADR-6, NFR-7)
`save_state` writes to a temp file in the same directory, then `os.replace` onto the target.

**Acceptance:** A check that interrupting between write and replace leaves the previous
state file intact and parseable.

### E-8 `pushed_at` prefilter and state schema (ADR-7)
State becomes `{repo: {"sha": ..., "pushed_at": ...}}`. Skip a repo before any per-repo API
call when its `pushed_at` from the repo listing is unchanged. Reads must tolerate the v1
bare-string form (treat as `sha` with no `pushed_at`) so upgrading does not rebuild all 17
repos.

**Acceptance:** Unit check covering three cases: v1 string state loads without error;
unchanged `pushed_at` performs zero per-repo API calls; changed `pushed_at` proceeds to the
Dockerfile and commit checks.

### E-9 Extend `test_app.py`, including the alert-prefix contract
Keep it stdlib `assert`-based, no framework, no network — cover the pure logic from E-3,
E-6 (SHA selection), E-7 and E-8. Retain the existing `image_name` cases with the corrected
username.

**Must include an assertion that the build-failure message starts with `build failed`**
(FR-15, ADR-11). This is the guard that stops a future reword of the log line from silently
disabling alerting — the most fragile coupling in the design.

**Acceptance:** `python test_app.py` exits 0 with no network access. Renaming the failure
prefix in `app.py` makes it fail.

### E-10 Delete `.github/workflows/build-and-push.yml`
Per ADR-3 and `design.md` §4. Keep `.github/PULL_REQUEST_TEMPLATE.md`.

**Acceptance:** File is gone; PR template remains.

---

## Section B — IaC-DevOps (deployment artifacts, alerting, migration)

### D-1 Dockerfile: pin `docker-cli`, drop the accidental build path (ADR-2)
Pin `docker-cli` to an explicit version rather than floating on alpine's latest. Do not add
the buildx plugin. `DOCKER_BUILDKIT` comes from the environment, not the image.

**Acceptance:** Rebuilding after an alpine base bump yields the same `docker --version`.

### D-2 `.env.template` (ADR-1, ADR-2)
Set `DOCKER_HOST=tcp://docker-lxc.localdomain:2375`. **Remove the stale "KNOWN RISK"
comment block** — EF-2/EF-4/EF-5 disprove it; replace with a one-line note that this is the
proven endpoint and that `unix:///var/run/docker.sock` plus a bind mount is the fallback.
Set `GITHUB_USERNAME=jjohnson83853`. Add `DOCKER_BUILDKIT=1` and `BUILD_TIMEOUT_SECONDS=1800`.
Keep the port-5000 note on `REGISTRY_URL` (C-2). Do not add `DOCKER_TLS_VERIFY` — see
`design.md` §4.

**Acceptance:** Every env var read by `app.py` is present and documented; no secret values
committed.

### D-3 `docker-compose.yml` (ADR-1, ADR-9, ADR-11)
Keep `env_file: .env`, the named volume on `/data`, and `restart: unless-stopped`. Add
label `com.centurylinklabs.watchtower.enable=false`. Add a log rotation limit. No `networks`
block is needed — D-2's host-published endpoint is reachable from the default bridge, which
is the point of ADR-1.

Set the container name to exactly **`docker-watcher`**: the alert rule's stream selector
matches `source_subsystem="docker-watcher"` (EF-11), so a generated or compose-prefixed name
would break alerting silently.

**Acceptance:** `docker compose config` validates and shows `container_name: docker-watcher`.
Deployed container resolves and reaches `tcp://docker-lxc.localdomain:2375` (`docker -H ...
version` from inside the container returns the EF-1 daemon).

### D-4 Verify a real end-to-end build before touching anything else
After the user's bootstrap build and `up -d`: confirm the watcher discovers ~17 repos,
builds at least one, and the image appears in `registry.localdomain:5000/v2/_catalog` with
both `:latest` and `:<short-sha>`.

**Acceptance:** Registry catalog and tag list show the new image; logs show no token
substring anywhere. **This task gates D-5 through D-12.**

### D-9 Confirm the watcher's logs actually reach Loki with the expected labels (A-4)
Query Loki for the new container's stream **before writing the alert rule** — the rule's
selector is worthless if the labels differ from the EF-11 pattern:

```
curl -sG 'http://192.168.50.72:3100/loki/api/v1/series' \
  --data-urlencode 'match[]={source_subsystem="docker-watcher"}' \
  --data-urlencode 'start=<ns>' --data-urlencode 'end=<ns>'
```

**Acceptance:** A stream is returned with `job="vector-logs"`, `host="192.168.50.61"`,
`source_system="docker_proxmox"`, `source_subsystem="docker-watcher"`. If any label differs,
**update the selector in D-10 to match reality** rather than forcing the assumed values.

### D-10 Author `alerts/build-failed.yaml` (ADR-11, FR-14/15/16)
One Grafana-managed alert rule against the existing Loki datasource, using the selector
confirmed in D-9:

```logql
sum(count_over_time(
  {job="vector-logs", host="192.168.50.61", source_subsystem="docker-watcher"}
  |= "build failed" [10m]
))
```

Condition `> 0`, evaluate every `5m`, `for: 0m`. Include a summary annotation naming the
service and linking to the Loki query. **Declare no custom contact point and no notification
policy** — the rule must fall through to Grafana's default policy (ADR-11 routing rationale).
Commit the file; nothing in the container reads it.

**Acceptance:** File is valid Grafana alert-rule provisioning YAML and contains no
credentials, no contact-point block and no datasource token.

### D-11 Apply the rule and verify it fires and delivers (C-7, A-5)
Apply `alerts/build-failed.yaml` to Grafana (`192.168.50.72:3000`, v13.1.3) with user-held
credentials — no Grafana token enters this repo or its `.env`.

Then verify end to end, not just that the rule loads: trigger a real failure (point the
watcher at a repo with a deliberately broken Dockerfile, or temporarily set
`BUILD_TIMEOUT_SECONDS=1` per E-5) and confirm the alert transitions to Alerting **and a
notification is actually delivered**.

**Acceptance:** The rule appears in Grafana alerting; a deliberately broken build moves it
to Alerting within ~2 evaluation cycles; **a notification arrives at the user's existing
channel.** If nothing arrives, the default notification policy has no working contact point
(A-5) — report that to the user rather than adding one, since choosing a channel is the
user's call, not ours.

### D-5 Parallel-run and confirm coverage (`design.md` §5 step 1)
Leave the fleet fully untouched. Confirm the watcher independently builds all five
runner-managed repos (`playlist-sync`, `swing-trade-mcp`, `schwab-mcp`, `mcp-market`,
`mcp-second-brain`).

**Acceptance:** Each of the five has a fresh `:<short-sha>` tag attributable to the watcher.
Duplicate builds during this window are expected and harmless.

### D-6 Remove per-repo workflows (§5 step 2 — must precede D-7)
One PR per runner-managed repo deleting `.github/workflows/build-and-push.yml`. Doing this
before stopping the runners is what prevents ~24h of hung queued jobs on every push.

**Acceptance:** All five PRs merged; no `build-and-push.yml` remains in any of the five.

### D-7 Tear down runner containers (§5 step 3)
Stop and remove the five `runner-repo-*` containers and their compose definitions.

**Acceptance:** `docker ps -a` on `docker-lxc` shows no `runner-repo-*` container.

### D-8 Revoke runner registrations and tokens (§5 step 4)
Delete each self-hosted runner in the corresponding GitHub repo settings and revoke the
five runner `ACCESS_TOKEN` PATs. **User action** — agents hold no credentials for this.

**Acceptance:** No self-hosted runners listed in any repo's settings; the five PATs are
revoked.

### D-12 Post-migration alert sanity check
After D-7, confirm the alert rule is still matching the right stream — tearing down five
containers changes what `vector-agent` is shipping and is a natural moment for a silent
regression.

**Acceptance:** The D-9 Loki series query still returns the watcher's stream, and the rule
is in Normal (not NoData) state.

**Ordering note:** D-9 through D-11 should be completed before D-6/D-7. Alerting is most
valuable precisely during the migration window, when the watcher becomes the sole builder.

---

## Section C — User actions (not agent-executable)

- **U-1** Issue the read-only-contents PAT for `GITHUB_USERNAME` (NFR-3) and populate `.env`.
- **U-2** Review and merge the PR; perform the bootstrap build and push of
  `registry.localdomain:5000/docker-watcher:latest` (C-5, FR-13).
- **U-3** Provide Grafana credentials for D-11, or apply `alerts/build-failed.yaml` yourself.
- **U-4** Execute D-8 (revoke runner registrations and PATs).
- **U-5** Optionally narrow the CLAUDE.md socket-proxy build warning per `design.md` §1.2 so
  it stops deterring correct designs.

*(The previous U-4 — amending the CLAUDE.md per-repo-workflow default — is done; see C-6.)*
