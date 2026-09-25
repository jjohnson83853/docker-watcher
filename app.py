"""Polls a GitHub account's repos, builds any with a Dockerfile on new commits to
each repo's default branch, and pushes the image to the configured registry."""
import json
import os
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

GITHUB_API = "https://api.github.com"

# Cap on how much of a failed docker build/push's output rides along in one
# log line - full output can run to thousands of lines (dependency installs
# etc.), and Loki lines have a size limit too.
MAX_BUILD_OUTPUT_CHARS = 4000


def log(level, event, **fields):
    print(json.dumps({"level": level, "event": event, **fields}), flush=True)


class BuildError(Exception):
    """A failed `docker build`/`docker push` with its captured output
    attached. Raised only for those two subprocess calls - ADR-5 still
    applies to `git clone`/`git rev-parse` (their argv/output can carry the
    tokenized clone URL), so those keep raising the bare
    CalledProcessError/TimeoutExpired with no output captured."""

    def __init__(self, returncode, output):
        self.returncode = returncode
        self.output = output
        super().__init__(f"exited {returncode}")


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


def _run_docker(cmd, timeout):
    """Run a docker build/push command with output captured, so a failure
    can carry it (BuildError) instead of just an exit code. Safe to capture
    in full - unlike git clone, neither this argv nor its output can contain
    the tokenized clone URL (ADR-5)."""
    try:
        subprocess.run(cmd, check=True, timeout=timeout, capture_output=True, text=True)
    except subprocess.CalledProcessError as e:
        raise BuildError(e.returncode, (e.stdout or "") + (e.stderr or "")) from e
    except subprocess.TimeoutExpired as e:
        raise BuildError("timeout", (e.stdout or "") + (e.stderr or "")) from e


def build_and_push(clone_url, branch, token, registry, image, timeout,
                   on_built=None):
    """Clone, strip .git, build, then push both tags. Returns the checked-out
    SHA. `on_built` runs after build success and before either tag push."""
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
        shutil.rmtree(os.path.join(tmpdir, ".git"), ignore_errors=True)

        tags = [f"{registry}/{image}:latest", f"{registry}/{image}:{sha[:7]}"]
        build_cmd = ["docker", "build"]
        for tag in tags:
            build_cmd += ["-t", tag]
        # The token must never reach a build context or layer history. BuildKit
        # secrets are unavailable through this deployment's Docker endpoint.
        build_cmd.append(tmpdir)
        _run_docker(build_cmd, timeout)
        if on_built is not None:
            on_built(sha)
        for tag in tags:
            _run_docker(["docker", "push", tag], timeout)
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


def graphify_refresh_persist_failure():
    log("error", "graphify_refresh", outcome="persist_failure",
        not_persisted=True)


def run_once(username, token, branch, registry, state_path, build_timeout=1800,
             graphify=None):
    state = load_state(state_path)
    for repo in list_owner_repos(username, token):
        name = repo["name"]
        repo_branch = branch or repo.get("default_branch") or "main"
        try:
            old_sha, old_pushed_at = state_entry(state, name)
            pushed_at = repo.get("pushed_at")
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

            def on_built(built_sha):
                prior = state.get(name)
                original = prior.copy() if isinstance(prior, dict) else prior
                try:
                    from graphify_refresh import record_target, target
                    generation = int(target(state, name) or 0) + 1
                    record_target(state, name, generation)
                    save_state(state_path, state)
                except Exception:
                    if original is None:
                        state.pop(name, None)
                    else:
                        state[name] = original
                    graphify_refresh_persist_failure()

            built_sha = build_and_push(
                repo["clone_url"], repo_branch, token, registry, image, build_timeout,
                on_built=on_built)
            entry = dict(state.get(name)) if isinstance(state.get(name), dict) else {}
            entry.update({"sha": built_sha, "pushed_at": pushed_at})
            state[name] = entry
            save_state(state_path, state)
            log("info", "build_success", repo=repo["full_name"], branch=repo_branch,
                sha=built_sha[:7], image=f"{registry}/{image}")
        except BuildError as e:
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                exit_code=e.returncode, output_tail=e.output[-MAX_BUILD_OUTPUT_CHARS:])
        except subprocess.CalledProcessError as e:
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                exit_code=e.returncode)
        except subprocess.TimeoutExpired:
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                exit_code="timeout")
        except Exception as e:
            log("error", "build failed", repo=repo["full_name"], branch=repo_branch,
                error_type=type(e).__name__)
    if graphify is not None:
        try:
            graphify.tick(state)
        except Exception:
            graphify_refresh_persist_failure()



def main():
    username = os.environ["GITHUB_USERNAME"]
    token = os.environ["GITHUB_TOKEN"]
    branch = os.environ.get("BRANCH") or None
    registry = os.environ.get("REGISTRY_URL", "registry.localdomain:5000")
    state_path = os.environ.get("STATE_FILE", "/data/state.json")
    interval = int(os.environ.get("POLL_INTERVAL_SECONDS", "300"))
    build_timeout = int(os.environ.get("BUILD_TIMEOUT_SECONDS", "1800"))
    heartbeat_path = os.environ.get("HEARTBEAT_FILE", "/data/heartbeat")
    from graphify_refresh import build_from_env
    graphify = build_from_env(
        os.environ, save=lambda current: save_state(state_path, current))

    while True:
        try:
            run_once(username, token, branch, registry, state_path, build_timeout,
                     graphify=graphify)
        except Exception as e:
            log("error", "poll_cycle_failed", error_type=type(e).__name__)
        finally:
            with open(heartbeat_path, "w"):
                pass
        time.sleep(interval)


if __name__ == "__main__":
    main()
