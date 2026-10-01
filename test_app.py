"""Pure-function checks, no network, no docker. python3 test_app.py exits 0."""
import json
import os
import subprocess
import tempfile
import urllib.error
import pytest

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
    assert app.save_state(path, {"a": "1"}) is True
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


def test_run_once_branch_follows_repo_default_unless_overridden():
    # EF-16/FR-13: owned repos disagree on their default branch. With no BRANCH
    # override each repo must be checked on its own default_branch, or every
    # master-default repo - docker-watcher included - is silently never built.
    repos = [
        {"name": "on-master", "full_name": "jjohnson83853/on-master",
         "pushed_at": "t1", "default_branch": "master",
         "clone_url": "https://github.com/jjohnson83853/on-master.git"},
        {"name": "on-main", "full_name": "jjohnson83853/on-main",
         "pushed_at": "t2", "default_branch": "main",
         "clone_url": "https://github.com/jjohnson83853/on-main.git"},
    ]
    # All three per-repo call sites must agree: the Dockerfile lookup, the
    # commit lookup, and the clone/build. A wrong branch at the latter two
    # means cloning and shipping the wrong code, so the test runs through to
    # build_and_push instead of stopping at the Dockerfile check.
    seen = []

    def fake_has_dockerfile(owner, repo, branch, token):
        seen.append(("has_dockerfile", repo, branch))
        return True

    def fake_get_latest_commit_sha(owner, repo, branch, token):
        seen.append(("get_latest_commit_sha", repo, branch))
        return "a" * 40

    def fake_build_and_push(clone_url, branch, token, registry, image, timeout,
                            on_built=None):
        seen.append(("build_and_push", image, branch))
        if on_built:
            on_built("b" * 40)
        return "b" * 40

    orig = (app.list_owner_repos, app.has_dockerfile,
            app.get_latest_commit_sha, app.build_and_push)
    app.list_owner_repos = lambda u, t: repos
    app.has_dockerfile = fake_has_dockerfile
    app.get_latest_commit_sha = fake_get_latest_commit_sha
    app.build_and_push = fake_build_and_push
    try:
        app.run_once("jjohnson83853", "tok", None, "registry.local:5000",
                     os.path.join(tempfile.mkdtemp(), "state.json"))
        assert seen == [
            ("has_dockerfile", "on-master", "master"),
            ("get_latest_commit_sha", "on-master", "master"),
            ("build_and_push", "on-master", "master"),
            ("has_dockerfile", "on-main", "main"),
            ("get_latest_commit_sha", "on-main", "main"),
            ("build_and_push", "on-main", "main"),
        ], seen

        seen.clear()
        app.run_once("jjohnson83853", "tok", "release", "registry.local:5000",
                     os.path.join(tempfile.mkdtemp(), "state.json"))
        assert seen == [
            ("has_dockerfile", "on-master", "release"),
            ("get_latest_commit_sha", "on-master", "release"),
            ("build_and_push", "on-master", "release"),
            ("has_dockerfile", "on-main", "release"),
            ("get_latest_commit_sha", "on-main", "release"),
            ("build_and_push", "on-main", "release"),
        ], seen
    finally:
        (app.list_owner_repos, app.has_dockerfile,
         app.get_latest_commit_sha, app.build_and_push) = orig


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


def _build_failed_lines(messages):
    """Raw log lines alerts/build-failed.yaml would fire on.

    Loki's `|= "build failed"` is a substring match over the whole line, so
    since logs went JSON both things must hold: the literal substring is
    still present in the serialized line, and it is there because `event`
    is "build failed" at error level - not by accident in some other field.
    """
    lines = []
    for m in messages:
        rec = json.loads(m)
        if rec.get("event") == "build failed":
            assert rec["level"] == "error", rec
            assert "build failed" in m, m  # the literal Loki filter matches
            lines.append(m)
    return lines


def test_run_once_build_failure_logs_build_failed_event():
    # E-4/E-9/FR-15: this is the literal interface contract alerts/build-failed.yaml
    # matches on. Renaming this event must fail this test.
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

    failure_lines = _build_failed_lines(messages)
    assert len(failure_lines) == 1, messages
    assert json.loads(failure_lines[0])["exit_code"] == 1, failure_lines
    # NFR-2: the tokenized clone URL is in the CalledProcessError argv and
    # must not reach the log line.
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

    failure_lines = _build_failed_lines(messages)
    assert len(failure_lines) == 1, messages
    assert json.loads(failure_lines[0])["exit_code"] == "timeout", failure_lines


def test_build_and_push_docker_build_failure_raises_build_error_with_output():
    # Previously a failed `docker build` surfaced as a bare CalledProcessError
    # with no output attached, so the eventual log line carried nothing but
    # an exit code - undiagnosable without shelling into the container.
    def fake_run(cmd, **kwargs):
        if cmd[:2] == ["docker", "build"]:
            raise subprocess.CalledProcessError(
                1, cmd, output="Step 3/5 : COPY extraction.py .\n",
                stderr="COPY failed: file not found in build context\n",
            )
        return _FakeCompletedProcess()

    orig_run = app.subprocess.run
    orig_mkdtemp = app.tempfile.mkdtemp
    orig_rmtree = app.shutil.rmtree
    app.subprocess.run = fake_run
    app.tempfile.mkdtemp = lambda: "/fake/tmp/dir"
    app.shutil.rmtree = lambda *a, **k: None
    try:
        try:
            app.build_and_push(
                "https://github.com/jjohnson83853/repo-a.git", "main", "sekret-token",
                "registry.local:5000", "repo-a", timeout=30,
            )
            assert False, "expected BuildError"
        except app.BuildError as e:
            assert e.returncode == 1
            assert "COPY failed" in e.output
            assert "Step 3/5" in e.output
    finally:
        app.subprocess.run = orig_run
        app.tempfile.mkdtemp = orig_mkdtemp
        app.shutil.rmtree = orig_rmtree


def test_run_once_docker_build_failure_logs_output_tail():
    # The other half: run_once must surface BuildError's captured output in
    # the structured "build failed" line, not just exit_code.
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
        raise app.BuildError(1, "COPY failed: file not found in build context\n")

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

    failure_lines = _build_failed_lines(messages)
    assert len(failure_lines) == 1, messages
    rec = json.loads(failure_lines[0])
    assert rec["exit_code"] == 1, rec
    assert "COPY failed" in rec["output_tail"], rec


def test_has_dockerfile_404_is_not_logged():
    # Bug fix: a 404 from the contents endpoint just means "no Dockerfile",
    # which is the normal outcome for most owned repos - ADR-8's "log every
    # non-200" must not apply here, or the logs drown in routine noise.
    messages = []
    orig_print = app.print

    def fake_urlopen(req, timeout=30):
        raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, None)

    orig_urlopen = app.urllib.request.urlopen
    app.urllib.request.urlopen = fake_urlopen
    app.print = lambda *a, **k: messages.append(" ".join(str(x) for x in a))
    try:
        result = app.has_dockerfile("jjohnson83853", "AbstractDAO", "main", "tok")
    finally:
        app.urllib.request.urlopen = orig_urlopen
        app.print = orig_print

    assert result is False
    assert messages == []


def test_has_dockerfile_500_is_still_logged():
    # A genuinely abnormal status at the same call site must still be loud.
    messages = []
    orig_print = app.print

    def fake_urlopen(req, timeout=30):
        raise urllib.error.HTTPError(req.full_url, 500, "Server Error", {}, None)

    orig_urlopen = app.urllib.request.urlopen
    app.urllib.request.urlopen = fake_urlopen
    app.print = lambda *a, **k: messages.append(" ".join(str(x) for x in a))
    try:
        result = app.has_dockerfile("jjohnson83853", "AbstractDAO", "main", "tok")
    finally:
        app.urllib.request.urlopen = orig_urlopen
        app.print = orig_print

    assert result is False
    assert any("500" in m for m in messages), messages


def test_gh_get_other_call_sites_still_log_404():
    # ADR-8 regression guard: a plain gh_get call (e.g. repo listing, commit
    # lookup) with no quiet_statuses must keep logging any non-200 as before.
    messages = []
    orig_print = app.print

    def fake_urlopen(req, timeout=30):
        raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, None)

    orig_urlopen = app.urllib.request.urlopen
    app.urllib.request.urlopen = fake_urlopen
    app.print = lambda *a, **k: messages.append(" ".join(str(x) for x in a))
    try:
        status, data = app.gh_get(f"{app.GITHUB_API}/user/repos", "tok")
    finally:
        app.urllib.request.urlopen = orig_urlopen
        app.print = orig_print

    assert status == 404 and data is None
    assert any("404" in m for m in messages), messages




def test_build_and_push_calls_target_hook_between_build_and_push():
    events = []

    def fake_run(cmd, **kwargs):
        if cmd[:2] == ["git", "rev-parse"]:
            return _FakeCompletedProcess("a" * 40)
        if cmd[:2] == ["docker", "build"]:
            events.append("build")
        elif cmd[:2] == ["docker", "push"]:
            events.append("push")
        return _FakeCompletedProcess()

    orig_run, orig_tmp, orig_rm = app.subprocess.run, app.tempfile.mkdtemp, app.shutil.rmtree
    app.subprocess.run = fake_run
    app.tempfile.mkdtemp = lambda: "/fake/clone"
    app.shutil.rmtree = lambda *a, **k: None
    try:
        app.build_and_push("https://example/repo", "main", "token", "registry",
                           "image", 30, on_built=lambda sha: events.append("target"))
    finally:
        app.subprocess.run, app.tempfile.mkdtemp, app.shutil.rmtree = orig_run, orig_tmp, orig_rm
    assert events == ["build", "target", "push", "push"]



def test_gh_get_and_pagination_are_offline(monkeypatch):
    class GoodResponse:
        status = 200
        def __enter__(self): return self
        def __exit__(self, *args): return None
        def read(self): return b'{"sha":"abc"}'

    monkeypatch.setattr(app.urllib.request, "urlopen", lambda *_a, **_k: GoodResponse())
    assert app.gh_get("https://api.test", "token") == (200, {"sha": "abc"})

    def http_error(*_args, **_kwargs):
        raise app.urllib.error.HTTPError("url", 403, "denied", {}, None)
    monkeypatch.setattr(app.urllib.request, "urlopen", http_error)
    assert app.gh_get("https://api.test", "token", quiet_statuses=(403,)) == (403, None)

    pages = [
        [{"owner": {"login": "alice"}, "name": str(i)} for i in range(100)],
        [{"owner": {"login": "ALICE"}, "name": "last"}],
    ]
    calls = []
    monkeypatch.setattr(app, "gh_get", lambda url, token: (calls.append(url) or (200, pages.pop(0))))
    owned = app.list_owner_repos("alice", "token")
    assert len(owned) == 101
    assert "page=2" in calls[-1]
    monkeypatch.setattr(app, "gh_get", lambda *_args: (200, {"sha": "abc"}))
    assert app.get_latest_commit_sha("a", "b", "main", "token") == "abc"


def test_build_failure_and_environment_poll_cycle(monkeypatch, tmp_path, capsys):
    def failed_run(*_args, **kwargs):
        raise app.subprocess.CalledProcessError(9, "docker", output="out", stderr="err")
    monkeypatch.setattr(app.subprocess, "run", failed_run)
    with pytest.raises(app.BuildError) as err:
        app._run_docker(["docker", "build"], 2)
    assert err.value.output == "outerr"

    monkeypatch.setattr(app, "list_owner_repos", lambda *_: [])
    state_path = tmp_path / "state.json"
    app.run_once("owner", "token", None, "registry", str(state_path))
    assert app.load_state(state_path) == {}
    app.save_state(state_path, {"repo": {"sha": "x"}})
    assert app.load_state(state_path) == {"repo": {"sha": "x"}}

    heartbeat = tmp_path / "heartbeat"
    monkeypatch.setenv("GITHUB_USERNAME", "owner")
    monkeypatch.setenv("GITHUB_TOKEN", "offline-token")
    monkeypatch.setenv("HEARTBEAT_FILE", str(heartbeat))
    monkeypatch.setattr(app, "run_once", lambda *_args, **_kwargs: None)
    class StopLoop(Exception): pass
    monkeypatch.setattr(app.time, "sleep", lambda _seconds: (_ for _ in ()).throw(StopLoop()))
    with pytest.raises(StopLoop):
        app.main()
    assert heartbeat.exists()
def test_docker_timeout_is_reported_as_build_error(monkeypatch):
    def timeout(*_args, **_kwargs):
        raise app.subprocess.TimeoutExpired("docker build", 1, output="partial", stderr="output")

    monkeypatch.setattr(app.subprocess, "run", timeout)
    with pytest.raises(app.BuildError) as error:
        app._run_docker(["docker", "build"], 1)
    assert error.value.returncode == "timeout"
    assert "partial" in error.value.output

if __name__ == "__main__":
    raise SystemExit(pytest.main(["-q", "--tb=short", "--no-cov", __file__]))
