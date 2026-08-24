"""Polls a GitHub account's repos, builds any with a Dockerfile on new main-branch
commits, and pushes the image to the configured registry."""
import json
import os
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

GITHUB_API = "https://api.github.com"


def log(level, event, **fields):
    print(json.dumps({"level": level, "event": event, **fields}), flush=True)


def gh_get(url, token, quiet_statuses=()):
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        # ADR-8: a non-200 must be loud, not collapsed into "no data" -
        # except statuses the caller says are an expected, routine outcome
        # (e.g. a 404 from the Dockerfile-contents check just means "no
        # Dockerfile"). Anything else (401/403/5xx/etc.) still logs here.
        if e.code not in quiet_statuses:
            log("error", "github_api_error", status=e.code, url=url)
        return e.code, None


def list_owner_repos(username, token):
    repos, page = [], 1
    while True:
        status, data = gh_get(
            f"{GITHUB_API}/user/repos?affiliation=owner&per_page=100&page={page}",
            token,
        )
        if status != 200 or not data:
            break
        repos.extend(data)
        if len(data) < 100:
            break
        page += 1
    owned = [r for r in repos if r["owner"]["login"].lower() == username.lower()]
    # ADR-8: log the count every cycle, including zero, so a misconfigured
    # token/username is visibly distinguishable from "nothing to do".
    log("info", "repos_found", count=len(owned))
    return owned


def has_dockerfile(owner, repo, branch, token):
    # A 404 here just means "this repo has no Dockerfile" - routine control
    # flow for most owned repos, not the kind of anomaly ADR-8 wants logged.
    status, _ = gh_get(
        f"{GITHUB_API}/repos/{owner}/{repo}/contents/Dockerfile?ref={branch}",
        token,
        quiet_statuses=(404,),
    )
    return status == 200


def get_latest_commit_sha(owner, repo, branch, token):
    status, data = gh_get(
        f"{GITHUB_API}/repos/{owner}/{repo}/commits/{branch}", token
    )
    return data["sha"] if status == 200 else None


def image_name(repo_full_name, username):
    """'username/my-repo' -> 'my-repo'"""
    prefix = f"{username}/"
    if repo_full_name.lower().startswith(prefix.lower()):
        return repo_full_name[len(prefix):]
    return repo_full_name


def state_entry(state, name):
    """Read one repo's state, tolerating the v1 bare-string shape (ADR-7)."""
    v = state.get(name)
    if v is None:
        return None, None
    if isinstance(v, str):
        return v, None
    return v.get("sha"), v.get("pushed_at")


def build_and_push(clone_url, branch, token, registry, image, timeout):
    """Clone, strip .git (ADR-4), build and push. Returns the SHA actually
    checked out (git rev-parse HEAD), which is what gets tagged and stored
    (FR-9) - never the pre-clone SHA from the API."""
    tmpdir = tempfile.mkdtemp()
    try:
        auth_url = clone_url.replace("https://", f"https://x-access-token:{token}@")
        subprocess.run(
            ["git", "clone", "--depth", "1", "--branch", branch, auth_url, tmpdir],
            check=True,
            timeout=timeout,
        )
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=tmpdir,
            check=True,
            timeout=timeout,
            capture_output=True,
            text=True,
        ).stdout.strip()
        # NFR-1: .git/config holds the tokenized clone URL; it must never be
        # part of the build context.
        shutil.rmtree(os.path.join(tmpdir, ".git"), ignore_errors=True)

        tags = [f"{registry}/{image}:latest", f"{registry}/{image}:{sha[:7]}"]
        build_cmd = ["docker", "build"]
        for tag in tags:
            build_cmd += ["-t", tag]
        # Pass token as a build-arg so Dockerfiles can consume it via ARG in
        # an intermediate stage (multi-stage keeps it out of the final image).
        # --mount=type=secret requires BuildKit, which is disabled (ADR-2).
        build_cmd += ["--build-arg", f"GITHUB_TOKEN={token}"]
        build_cmd.append(tmpdir)
        subprocess.run(build_cmd, check=True, timeout=timeout)
        for tag in tags:
            subprocess.run(["docker", "push", tag], check=True, timeout=timeout)
        return sha
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def load_state(path):
    if os.path.exists(path):
        with open(path) as f:
            return json.load(f)
    return {}


def save_state(path, state):
    """Atomic write (NFR-7): temp file in the same directory, then os.replace."""
    d = os.path.dirname(path) or "."
    os.makedirs(d, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(dir=d)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(state, f)
        os.replace(tmp_path, path)
    except Exception:
        os.unlink(tmp_path)
        raise


def run_once(username, token, branch, registry, state_path, build_timeout=1800):
    state = load_state(state_path)
    for repo in list_owner_repos(username, token):
        name = repo["name"]
        # EF-16: owned repos don't agree on a default branch - docker-watcher,
        # send-loki, subtitles and amazon_price_scanner default to master, the
        # rest to main. A single global BRANCH silently skips whichever half it
        # doesn't name, including this repo itself (FR-13 self-build). Unset
        # BRANCH (the default) follows each repo's own default branch; setting
        # it still forces one branch everywhere.
        repo_branch = branch or repo.get("default_branch") or "main"
        try:
            old_sha, old_pushed_at = state_entry(state, name)
            pushed_at = repo.get("pushed_at")
            # ADR-7: skip the per-repo Dockerfile/commit lookups entirely
            # when nothing has moved since last cycle.
            if pushed_at and pushed_at == old_pushed_at:
                continue
            if not has_dockerfile(username, name, repo_branch, token):
                continue
            sha = get_latest_commit_sha(username, name, repo_branch, token)
            if not sha or sha == old_sha:
                continue
            image = image_name(repo["full_name"], username)
            log("info", "build_started", repo=repo["full_name"], branch=repo_branch,
                sha=sha[:7], image=f"{registry}/{image}")
            built_sha = build_and_push(
                repo["clone_url"], repo_branch, token, registry, image, build_timeout
            )
            state[name] = {"sha": built_sha, "pushed_at": pushed_at}
            save_state(state_path, state)  # FR-10: only after a successful push
            log("info", "build_success", repo=repo["full_name"], branch=repo_branch,
                sha=built_sha[:7], image=f"{registry}/{image}")
        except subprocess.CalledProcessError as e:
            # ADR-5/NFR-2: never render argv (it contains the tokenized clone
            # URL) - log repo name and returncode only.
            # alerts/build-failed.yaml matches on the "build failed" prefix.
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                exit_code=e.returncode)
        except subprocess.TimeoutExpired:
            # NFR-6: a hung build is a failed build and must alert too.
            # alerts/build-failed.yaml matches on the "build failed" prefix.
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                exit_code="timeout")
        except Exception as e:
            # ADR-6: broadened from CalledProcessError-only so one repo's
            # failure (e.g. a URLError from a flaky API call) can't end the
            # cycle. alerts/build-failed.yaml matches on the "build failed" prefix.
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                error_type=type(e).__name__)


def main():
    username = os.environ["GITHUB_USERNAME"]
    token = os.environ["GITHUB_TOKEN"]
    # Empty/unset means "each repo's own default branch" (EF-16) - a set value
    # is an override that forces the same branch for every repo.
    branch = os.environ.get("BRANCH") or None
    registry = os.environ.get("REGISTRY_URL", "registry.localdomain:5000")
    state_path = os.environ.get("STATE_FILE", "/data/state.json")
    interval = int(os.environ.get("POLL_INTERVAL_SECONDS", "300"))
    build_timeout = int(os.environ.get("BUILD_TIMEOUT_SECONDS", "1800"))

    while True:
        try:
            run_once(username, token, branch, registry, state_path, build_timeout)
        except Exception as e:
            log("error", "poll_cycle_failed", error_type=type(e).__name__)
        time.sleep(interval)


if __name__ == "__main__":
    main()
