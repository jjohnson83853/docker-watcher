import json
import socket
from http.client import HTTPException
from unittest.mock import Mock

import pytest

import app
import graphify_refresh as refresh


class _Response:
    def __init__(self, status=200, body=b""):
        self.status = status
        self.body = body

    def read(self):
        return self.body


class _Connection:
    def __init__(self, _host, timeout):
        self.timeout = timeout
        self.response = _Response()
        self.requests = []
        self.closed = False

    def request(self, method, path, body=None, headers=None):
        self.requests.append((method, path, body, headers))

    def getresponse(self):
        return self.response

    def close(self):
        self.closed = True


def test_docker_client_validates_hosts_and_negotiates_api_once(monkeypatch):
    with pytest.raises(ValueError, match="unsupported"):
        refresh.DockerClient("http://docker")
    with pytest.raises(ValueError, match="empty unix"):
        refresh.DockerClient("unix://")
    with pytest.raises(ValueError, match="empty tcp"):
        refresh.DockerClient("tcp://")

    connections = []

    class Connection(_Connection):
        def __init__(self, host, timeout):
            super().__init__(host, timeout)
            self.response = _Response(200, b'{"ApiVersion":"1.44"}')
            connections.append(self)

    monkeypatch.setattr(refresh.http.client, "HTTPConnection", Connection)
    client = refresh.DockerClient("tcp://docker.example:2375/path", request_timeout=3)
    assert client._prefix() == "/v1.44"
    assert client._prefix() == "/v1.44"
    assert len(connections) == 1
    assert connections[0].timeout == 3
    assert connections[0].closed


def test_docker_client_request_handles_json_empty_invalid_and_errors(monkeypatch):
    replies = iter([
        _Response(200, b'{"ok":true}'), _Response(200, b""),
        _Response(200, b"not-json"), _Response(503),
    ])
    made = []

    class Connection(_Connection):
        def __init__(self, host, timeout):
            super().__init__(host, timeout)
            self.response = next(replies)
            made.append(self)

    monkeypatch.setattr(refresh.http.client, "HTTPConnection", Connection)
    client = refresh.DockerClient("tcp://docker:2375")
    assert client.request("POST", "/work", {"ok": True}) == (200, {"ok": True})
    assert json.loads(made[0].requests[0][2]) == {"ok": True}
    assert client.request("GET", "/empty")[1] == {}
    assert client.request("GET", "/invalid")[1] == {}
    with pytest.raises(refresh.DockerAPIError) as err:
        client.request("GET", "/failed")
    assert err.value.status == 503
    assert made[-1].closed

    class Broken(_Connection):
        def request(self, *args, **kwargs):
            raise OSError("private host detail")

    monkeypatch.setattr(refresh.http.client, "HTTPConnection", Broken)
    with pytest.raises(refresh.DockerUnreachable):
        client.request("GET", "/unreachable")


def test_docker_client_exec_contract_and_invalid_responses(monkeypatch):
    replies = iter([
        _Response(200, b'{"ApiVersion":"1.43"}'),
        _Response(200, b'{"Id":"abcdef123456"}'),
        _Response(200, b'{"Id":"fedcba654321"}'),
        _Response(200, b'{"Running":true,"ExitCode":null}'),
        _Response(200, b'{"Running":false,"ExitCode":0}'),
        _Response(200, b'{"Id":"bad"}'),
        _Response(200, b'{"Id":"bad"}'),
        _Response(200, b'{"Running":"yes","ExitCode":0}'),
    ])
    calls = []

    class Connection(_Connection):
        def __init__(self, host, timeout):
            super().__init__(host, timeout)
            self.response = next(replies)
            calls.append(self)

    monkeypatch.setattr(refresh.http.client, "HTTPConnection", Connection)
    client = refresh.DockerClient("tcp://docker:2375")
    container_id = client.container_id()
    exec_id = client.exec_create(container_id)
    assert client.exec_inspect(exec_id) == (True, None)
    assert client.exec_inspect(exec_id) == (False, 0)
    assert json.loads(calls[2].requests[0][2]) == {
        "AttachStdin": False, "AttachStdout": True, "AttachStderr": True,
        "Tty": False, "Cmd": refresh.GRAPHIFY_ARGV, "Env": refresh.GRAPHIFY_ENV,
    }
    with pytest.raises(refresh.DockerAPIError):
        client.container_id()
    with pytest.raises(refresh.DockerAPIError):
        client.exec_create("abcdef123456")
    with pytest.raises(refresh.DockerAPIError):
        client.exec_inspect("abcdef123456")


def test_safe_log_fields_and_retry_backoff_are_bounded(capsys):
    refresh.log(
        "debug", "not-real-outcome", repo="../secret", repos=["ok_repo", "bad repo"],
        exec_id="has spaces", status=99, reason="bad", not_persisted="yes",
        retry_delay=refresh.RETRY_CAP_SECONDS + 20, exit_code=700,
    )
    record = json.loads(capsys.readouterr().out)
    assert record["level"] == "error"
    assert record["outcome"] == "unresolved_exec"
    assert record["repo"] == "unknown"
    assert record["repos"] == ["ok_repo", "unknown"]
    assert "exec_id" not in record
    assert record["status"] == record["exit_code"] == "unknown"
    assert record["retry_delay"] == refresh.RETRY_CAP_SECONDS
    assert refresh.safe_exec_id("abc123") is None
    assert refresh.safe_status(True) == "unknown"
    assert [refresh.retry_delay(n) for n in (0, 1, 2, 6, 20)] == [60, 60, 120, 1800, 1800]


def test_dirty_retry_and_legacy_state_transitions():
    state = {"legacy": "old", "a": {"target": 2, "completed": 1, "retry_due": 10},
             "b": {"target": 4, "completed": 4}, refresh.RESERVED_KEY: {"target": 1}}
    assert refresh.dirty_repos(state) == ["a"]
    assert refresh.eligible_repos(state, 9) == []
    assert refresh.eligible_repos(state, 10) == ["a"]
    assert refresh.ensure_entry(state, "legacy")["sha"] == "old"
    refresh.record_target(state, "legacy", 3)
    assert refresh.target(state, "legacy") == 3
    assert refresh.completed(state, "absent") is None
    assert refresh.failures(state, "absent") == 0
    assert refresh.retry_due(state, "absent") is None


class _RefreshClient:
    def __init__(self, inspect_result=(False, 0), create_error=None, start_error=None):
        self.inspect_result = inspect_result
        self.create_error = create_error
        self.start_error = start_error
        self.calls = []

    def container_id(self):
        self.calls.append("container")
        return "a" * 12

    def exec_create(self, cid):
        self.calls.append(("create", cid))
        if self.create_error:
            raise self.create_error
        return "b" * 12

    def exec_start(self, eid, on_status, cancel, transport):
        self.calls.append(("start", eid))
        if self.start_error:
            if isinstance(self.start_error, refresh.DockerAPIError):
                on_status(("rejected", self.start_error.status))
            else:
                on_status(("transport_error", None))
            raise self.start_error
        on_status(("started", None))
        return _EmptyBody()

    def exec_inspect(self, eid):
        self.calls.append(("inspect", eid))
        if isinstance(self.inspect_result, Exception):
            raise self.inspect_result
        return self.inspect_result


class _EmptyBody:
    def read(self, _size, _cancel):
        return b""

    def close(self):
        pass


class _AttachedTransport:
    def cancel(self):
        pass


class _CaptureWorker:
    def __init__(self, start, cancel_callback=None):
        self._start = start
        self._status = ("pending", None)
        self._alive = False

    def start(self):
        self._alive = True
        try:
            self._body = self._start(self._publish, _NeverCancel())
        except Exception:
            pass
        self._alive = False

    def _publish(self, status):
        self._status = status

    def status(self):
        return self._status

    def result(self):
        return "unavailable"

    def is_alive(self):
        return self._alive

    def cancel(self):
        body = getattr(self, "_body", None)
        if body is not None:
            body.close()


class _NeverCancel:
    def is_set(self):
        return False


def _controller(client, **kwargs):
    return refresh.GraphifyRefresh(
        client,
        capture_worker_factory=_CaptureWorker,
        attached_exec_factory=lambda _client: _AttachedTransport(),
        **kwargs,
    )


def test_refresh_happy_path_persists_before_start_and_acknowledges():
    saved = []
    client = _RefreshClient()
    graph = _controller(client, now=lambda: 100, save=lambda state: saved.append(json.loads(json.dumps(state))) or True)
    state = {"repo": {"target": 3, "completed": 2}}
    graph.tick(state)
    assert client.calls == ["container", ("create", "a" * 12), ("start", "b" * 12)]
    assert len(saved) == 3
    assert saved[0][refresh.RESERVED_KEY]["attempt"]["phase"] == "pre_create"
    assert saved[1][refresh.RESERVED_KEY]["attempt"]["phase"] == "pre_start"
    assert saved[2][refresh.RESERVED_KEY]["attempt"]["start_requested"] is True
    graph.tick(state)
    assert client.calls[-1] == ("inspect", "b" * 12)
    assert state["repo"]["completed"] == 3
    assert graph._attempt(state) is None
    assert len(saved) == 5

def test_refresh_uses_atomic_state_save_as_success(tmp_path):
    state_path = tmp_path / "state.json"
    client = _RefreshClient()
    graph = _controller(
        client,
        now=lambda: 100,
        save=lambda state: app.save_state(str(state_path), state),
    )
    state = {"repo": {"target": 1}}

    graph.tick(state)

    assert client.calls == ["container", ("create", "a" * 12), ("start", "b" * 12)]
    saved = app.load_state(state_path)
    assert saved[refresh.RESERVED_KEY]["attempt"]["start_requested"] is True

def test_refresh_creation_failure_schedules_retry_without_external_start(capsys):
    client = _RefreshClient(create_error=refresh.DockerAPIError(503))
    graph = _controller(client, now=lambda: 50)
    state = {"repo": {"target": 1}}
    graph.tick(state)
    assert client.calls == ["container", ("create", "a" * 12)]
    assert state["repo"]["failures"] == 1
    assert state["repo"]["retry_due"] == 110
    assert graph._attempt(state) is None
    assert json.loads(capsys.readouterr().out.splitlines()[-1])["outcome"] == "retry_scheduled"


def test_refresh_start_rejection_recovers_immediately_on_nullable_inspect(capsys):
    client = _RefreshClient(
        inspect_result=(False, None), start_error=refresh.DockerAPIError(400))
    graph = _controller(client, now=lambda: 100)
    state = {"repo": {"target": 9}}
    graph.tick(state)
    graph.tick(state)
    assert state["repo"]["failures"] == 1
    assert state["repo"]["retry_due"] == 160
    assert graph._attempt(state) is None
    assert client.calls[-1] == ("inspect", "b" * 12)
    outcomes = [json.loads(line)["outcome"] for line in capsys.readouterr().out.splitlines()]
    assert refresh.RETRY_SCHEDULED in outcomes
    assert refresh.EXEC_FAILED not in outcomes


def test_refresh_unsaved_attempt_suppresses_create_and_unknown_inspection_blocks():
    client = _RefreshClient()
    graph = _controller(client, now=lambda: 100, save=lambda _state: False)
    state = {"repo": {"target": 1}}
    graph.tick(state)
    assert client.calls == []
    assert graph._attempt(state) is None

    client = _RefreshClient(inspect_result=refresh.DockerUnreachable())
    graph = _controller(client, now=lambda: 100)
    state = {"repo": {"target": 1}, refresh.RESERVED_KEY: {"attempt": {
        "exec_id": "b" * 12, "start_requested": True,
        "fallback_deadline": 0, "snapshot": {"repo": 1},
    }}}
    graph.tick(state)
    assert client.calls == [("inspect", "b" * 12)]
    assert graph._attempt(state) is not None


def test_refresh_build_from_env_rejects_invalid_config(capsys):
    assert refresh.build_from_env({}) is None
    assert refresh.build_from_env({"DOCKER_HOST": "tcp://host", "GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS": "nan"}) is None
    assert refresh.build_from_env({"DOCKER_HOST": "http://host"}) is None
    assert json.loads(capsys.readouterr().out.splitlines()[-1])["reason"] == "invalid_config"


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
def test_refresh_running_exec_waits_and_rejected_duplicate_start_is_inspected(capsys):
    client = _RefreshClient(inspect_result=(True, None))
    graph = _controller(client, now=lambda: 100)
    state = {"repo": {"target": 1}, refresh.RESERVED_KEY: {"attempt": {
        "exec_id": "b" * 12, "start_requested": True,
        "fallback_deadline": 0, "snapshot": {"repo": 1},
    }}}
    graph.tick(state)
    assert graph._attempt(state) is not None
    assert client.calls == [("inspect", "b" * 12)]
    assert json.loads(capsys.readouterr().out.splitlines()[-1])["outcome"] == "exec_running"

    client = _RefreshClient(inspect_result=(False, 0), start_error=refresh.DockerAPIError(409))
    graph = _controller(client, now=lambda: 100)
    state = {"repo": {"target": 2}, refresh.RESERVED_KEY: {"attempt": {
        "exec_id": "b" * 12, "container_id": "a" * 12,
        "start_requested": False, "fallback_deadline": 0, "snapshot": {"repo": 2},
    }}}
    graph.tick(state)
    graph.tick(state)
    assert state["repo"]["completed"] == 2
    assert graph._attempt(state) is None
    assert client.calls == [("start", "b" * 12), ("inspect", "b" * 12)]


def test_docker_timeout_is_reported_as_build_error(monkeypatch):
    def timeout(*_args, **_kwargs):
        raise app.subprocess.TimeoutExpired("docker build", 1, output="partial", stderr="output")

    monkeypatch.setattr(app.subprocess, "run", timeout)
    with pytest.raises(app.BuildError) as error:
        app._run_docker(["docker", "build"], 1)
    assert error.value.returncode == "timeout"
    assert "partial" in error.value.output

