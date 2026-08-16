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


def gh_get(url, token):
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
    return [r for r in repos if r["owner"]["login"].lower() == username.lower()]


def has_dockerfile(owner, repo, branch, token):
    status, _ = gh_get(
        f"{GITHUB_API}/repos/{owner}/{repo}/contents/Dockerfile?ref={branch}", token
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


def build_and_push(clone_url, branch, token, registry, image, sha):
    tmpdir = tempfile.mkdtemp()
    try:
        auth_url = clone_url.replace("https://", f"https://x-access-token:{token}@")
        subprocess.run(
            ["git", "clone", "--depth", "1", "--branch", branch, auth_url, tmpdir],
            check=True,
        )
        tags = [f"{registry}/{image}:latest", f"{registry}/{image}:{sha[:7]}"]
        build_cmd = ["docker", "build"]
        for tag in tags:
            build_cmd += ["-t", tag]
        build_cmd.append(tmpdir)
        subprocess.run(build_cmd, check=True)
        for tag in tags:
            subprocess.run(["docker", "push", tag], check=True)
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def load_state(path):
    if os.path.exists(path):
        with open(path) as f:
            return json.load(f)
    return {}


def save_state(path, state):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(state, f)


def run_once(username, token, branch, registry, state_path):
    state = load_state(state_path)
    for repo in list_owner_repos(username, token):
        name = repo["name"]
        try:
            if not has_dockerfile(username, name, branch, token):
                continue
            sha = get_latest_commit_sha(username, name, branch, token)
            if not sha or state.get(name) == sha:
                continue
            image = image_name(repo["full_name"], username)
            print(f"building {name} @ {sha[:7]} -> {registry}/{image}", flush=True)
            build_and_push(repo["clone_url"], branch, token, registry, image, sha)
            state[name] = sha
            save_state(state_path, state)
        except subprocess.CalledProcessError as e:
            print(f"build failed for {name}: {e}", flush=True)


def main():
    username = os.environ["GITHUB_USERNAME"]
    token = os.environ["GITHUB_TOKEN"]
    branch = os.environ.get("BRANCH", "main")
    registry = os.environ.get("REGISTRY_URL", "registry.localdomain:5000")
    state_path = os.environ.get("STATE_FILE", "/data/state.json")
    interval = int(os.environ.get("POLL_INTERVAL_SECONDS", "300"))

    while True:
        try:
            run_once(username, token, branch, registry, state_path)
        except Exception as e:
            print(f"poll cycle failed: {e}", flush=True)
        time.sleep(interval)


if __name__ == "__main__":
    main()
