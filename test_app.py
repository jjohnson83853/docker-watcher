"""Pure-function checks, no network, no docker. python3 test_app.py exits 0."""
import json
import os
import subprocess
import tempfile
import urllib.error

import app


def test_image_name():
    assert app.image_name("jjohnson83853/my-cool-app", "jjohnson83853") == "my-cool-app"
    assert app.image_name("jjohnson83853/My-App", "jjohnson83853") == "My-App"
    assert app.image_name("someorg/repo", "jjohnson83853") == "someorg/repo"


def test_state_entry_v1_bare_string():
    # E-8: upgrading must not choke on the v1 shape or force a rebuild-all.
    state = {"repo-a": "abc123"}
    assert app.state_entry(state, "repo-a") == ("abc123", None)
    assert app.state_entry(state, "missing") == (None, None)


def test_state_entry_v2_dict():
    state = {"repo-a": {"sha": "abc123", "pushed_at": "2026-01-01T00:00:00Z"}}
    assert app.state_entry(state, "repo-a") == ("abc123", "2026-01-01T00:00:00Z")


def test_load_state_tolerates_v1_file():
    tmp = tempfile.mkstemp()[1]
    with open(tmp, "w") as f:
        json.dump({"repo-a": "abc123"}, f)
    state = app.load_state(tmp)
    assert state == {"repo-a": "abc123"}
    assert app.state_entry(state, "repo-a") == ("abc123", None)


def test_save_state_is_atomic_on_crash():
    # E-7: an interruption between write and replace must leave the previous
    # state file intact and parseable, not truncated.
    path = tempfile.mkstemp()[1]
    app.save_state(path, {"a": "1"})

    orig_replace = app.os.replace

    def boom(*a, **k):
        raise RuntimeError("simulated crash before replace")

    app.os.replace = boom
    try:
        try:
            app.save_state(path, {"a": "2", "b": "3"})
            raise AssertionError("expected the simulated crash to propagate")
        except RuntimeError:
            pass
    finally:
        app.os.replace = orig_replace

    with open(path) as f:
        data = json.load(f)
    assert data == {"a": "1"}


def test_run_once_one_repo_url_error_does_not_abort_cycle():
    # E-3: a URLError from one repo's API call must not stop the cycle.
    processed = []

    def fake_list_owner_repos(username, token):
        return [
            {"name": "repo-a", "full_name": "jjohnson83853/repo-a", "pushed_at": "t1",
             "clone_url": "https://github.com/jjohnson83853/repo-a.git"},
            {"name": "repo-b", "full_name": "jjohnson83853/repo-b", "pushed_at": "t2",
             "clone_url": "https://github.com/jjohnson83853/repo-b.git"},
        ]

    def fake_has_dockerfile(owner, repo, branch, token):
        if repo == "repo-a":
            raise urllib.error.URLError("connection refused")
        processed.append(repo)
        return False  # skip past build for repo-b, we just care it was reached

    orig_list = app.list_owner_repos
    orig_has = app.has_dockerfile
    app.list_owner_repos = fake_list_owner_repos
    app.has_dockerfile = fake_has_dockerfile
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000",
                      os.path.join(tempfile.mkdtemp(), "state.json"))
    finally:
        app.list_owner_repos = orig_list
        app.has_dockerfile = orig_has

    assert processed == ["repo-b"]


def test_run_once_unchanged_pushed_at_skips_api_calls():
    # E-8 case 2: zero per-repo API calls when pushed_at is unchanged.
    state_path = tempfile.mkstemp()[1]
    app.save_state(state_path, {"repo-a": {"sha": "old-sha", "pushed_at": "same"}})

    def fake_list_owner_repos(username, token):
        return [{"name": "repo-a", "full_name": "jjohnson83853/repo-a",
                  "pushed_at": "same",
                  "clone_url": "https://github.com/jjohnson83853/repo-a.git"}]

    def fail(*a, **k):
        raise AssertionError("should not be called when pushed_at is unchanged")

    orig_list, orig_has, orig_sha = app.list_owner_repos, app.has_dockerfile, app.get_latest_commit_sha
    app.list_owner_repos = fake_list_owner_repos
    app.has_dockerfile = fail
    app.get_latest_commit_sha = fail
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000", state_path)
    finally:
        app.list_owner_repos = orig_list
        app.has_dockerfile = orig_has
        app.get_latest_commit_sha = orig_sha


def test_run_once_changed_pushed_at_proceeds_to_checks():
    # E-8 case 3: changed pushed_at reaches the Dockerfile and commit checks.
    state_path = tempfile.mkstemp()[1]
    app.save_state(state_path, {"repo-a": {"sha": "old-sha", "pushed_at": "old"}})

    reached = {"dockerfile": False, "commit": False}

    def fake_list_owner_repos(username, token):
        return [{"name": "repo-a", "full_name": "jjohnson83853/repo-a",
                  "pushed_at": "new",
                  "clone_url": "https://github.com/jjohnson83853/repo-a.git"}]

    def fake_has_dockerfile(owner, repo, branch, token):
        reached["dockerfile"] = True
        return True

    def fake_get_latest_commit_sha(owner, repo, branch, token):
        reached["commit"] = True
        return "old-sha"  # unchanged -> no build, but checks were reached

    orig_list, orig_has, orig_sha = app.list_owner_repos, app.has_dockerfile, app.get_latest_commit_sha
    app.list_owner_repos = fake_list_owner_repos
    app.has_dockerfile = fake_has_dockerfile
    app.get_latest_commit_sha = fake_get_latest_commit_sha
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000", state_path)
    finally:
        app.list_owner_repos = orig_list
        app.has_dockerfile = orig_has
        app.get_latest_commit_sha = orig_sha

    assert reached == {"dockerfile": True, "commit": True}


class _FakeCompletedProcess:
    def __init__(self, stdout=""):
        self.stdout = stdout


def test_build_and_push_uses_rev_parse_sha_not_precall_sha():
    # E-6: tag/state SHA must be git rev-parse HEAD of the clone, not any
    # SHA observed before cloning. No real git/docker/network involved.
    calls = []
    rev_parse_sha = "deadbeefcafefeed1234567890abcdef12345678"

    def fake_run(cmd, **kwargs):
        calls.append(cmd)
        if cmd[:2] == ["git", "rev-parse"]:
            return _FakeCompletedProcess(stdout=rev_parse_sha + "\n")
        return _FakeCompletedProcess()

    orig_run = app.subprocess.run
    orig_mkdtemp = app.tempfile.mkdtemp
    orig_rmtree = app.shutil.rmtree
    app.subprocess.run = fake_run
    app.tempfile.mkdtemp = lambda: "/fake/tmp/dir"
    app.shutil.rmtree = lambda *a, **k: None
    try:
        sha = app.build_and_push(
            "https://github.com/jjohnson83853/repo-a.git", "main", "sekret-token",
            "registry.local:5000", "repo-a", timeout=30,
        )
    finally:
        app.subprocess.run = orig_run
        app.tempfile.mkdtemp = orig_mkdtemp
        app.shutil.rmtree = orig_rmtree

    assert sha == rev_parse_sha
    build_cmd = next(c for c in calls if c[:2] == ["docker", "build"])
    assert f"registry.local:5000/repo-a:{rev_parse_sha[:7]}" in build_cmd
    assert "sekret-token" not in " ".join(build_cmd)


def test_run_once_build_failure_message_starts_with_build_failed():
    # E-4/E-9/FR-15: this is the literal interface contract alerts/build-failed.yaml
    # matches on. Renaming this prefix must fail this test.
    state_path = os.path.join(tempfile.mkdtemp(), "state.json")

    def fake_list_owner_repos(username, token):
        return [{"name": "repo-a", "full_name": "jjohnson83853/repo-a",
                  "pushed_at": "t1",
                  "clone_url": "https://github.com/jjohnson83853/repo-a.git"}]

    def fake_has_dockerfile(owner, repo, branch, token):
        return True

    def fake_get_latest_commit_sha(owner, repo, branch, token):
        return "abc1234"

    def fake_build_and_push(*a, **k):
        raise subprocess.CalledProcessError(1, ["git", "clone", "https://x-access-token:sekret@github.com/..."])

    messages = []
    orig_print = print

    def fake_print(*args, **kwargs):
        messages.append(" ".join(str(a) for a in args))

    orig_list, orig_has, orig_sha, orig_build = (
        app.list_owner_repos, app.has_dockerfile, app.get_latest_commit_sha, app.build_and_push
    )
    app.list_owner_repos = fake_list_owner_repos
    app.has_dockerfile = fake_has_dockerfile
    app.get_latest_commit_sha = fake_get_latest_commit_sha
    app.build_and_push = fake_build_and_push
    app.print = fake_print
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000", state_path)
    finally:
        app.list_owner_repos = orig_list
        app.has_dockerfile = orig_has
        app.get_latest_commit_sha = orig_sha
        app.build_and_push = orig_build
        app.print = orig_print

    failure_lines = [m for m in messages if m.startswith("build failed")]
    assert len(failure_lines) == 1, messages
    assert "sekret" not in failure_lines[0]


def test_run_once_timeout_expired_also_reports_build_failed():
    # E-5: a hung build is a failed build and must alert too.
    state_path = os.path.join(tempfile.mkdtemp(), "state.json")

    def fake_list_owner_repos(username, token):
        return [{"name": "repo-a", "full_name": "jjohnson83853/repo-a",
                  "pushed_at": "t1",
                  "clone_url": "https://github.com/jjohnson83853/repo-a.git"}]

    def fake_has_dockerfile(owner, repo, branch, token):
        return True

    def fake_get_latest_commit_sha(owner, repo, branch, token):
        return "abc1234"

    def fake_build_and_push(*a, **k):
        raise subprocess.TimeoutExpired(cmd=["docker", "build"], timeout=1)

    messages = []

    def fake_print(*args, **kwargs):
        messages.append(" ".join(str(a) for a in args))

    orig_print = print
    orig_list, orig_has, orig_sha, orig_build = (
        app.list_owner_repos, app.has_dockerfile, app.get_latest_commit_sha,
        app.build_and_push
    )
    app.list_owner_repos = fake_list_owner_repos
    app.has_dockerfile = fake_has_dockerfile
    app.get_latest_commit_sha = fake_get_latest_commit_sha
    app.build_and_push = fake_build_and_push
    app.print = fake_print
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000", state_path)
    finally:
        app.list_owner_repos = orig_list
        app.has_dockerfile = orig_has
        app.get_latest_commit_sha = orig_sha
        app.build_and_push = orig_build
        app.print = orig_print

    assert any(m.startswith("build failed") for m in messages), messages


if __name__ == "__main__":
    test_image_name()
    test_state_entry_v1_bare_string()
    test_state_entry_v2_dict()
    test_load_state_tolerates_v1_file()
    test_save_state_is_atomic_on_crash()
    test_run_once_one_repo_url_error_does_not_abort_cycle()
    test_run_once_unchanged_pushed_at_skips_api_calls()
    test_run_once_changed_pushed_at_proceeds_to_checks()
    test_build_and_push_uses_rev_parse_sha_not_precall_sha()
    test_run_once_build_failure_message_starts_with_build_failed()
    test_run_once_timeout_expired_also_reports_build_failed()
    print("ok")
