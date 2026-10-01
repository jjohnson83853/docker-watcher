import json
import os
import tempfile
import app


class _RefreshClient:
    def __init__(self):
        self.creates = []
        self.starts = []
        self.inspects = []
        self.inspect_results = []

    def container_id(self):
        return "c" * 64

    def exec_create(self, cid):
        self.creates.append(cid)
        return f"{len(self.creates):064x}"

    def exec_start(self, eid, on_status, cancel, transport):
        self.starts.append(eid)
        on_status(("started", None))
        return _EmptyBody()

    def exec_inspect(self, eid):
        self.inspects.append(eid)
        result = self.inspect_results.pop(0)
        if isinstance(result, Exception):
            raise result
        return result


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
    from graphify_refresh import GraphifyRefresh
    return GraphifyRefresh(
        client,
        capture_worker_factory=_CaptureWorker,
        attached_exec_factory=lambda _client: _AttachedTransport(),
        **kwargs,
    )


class _RefreshClock:
    def __init__(self, value=1000):
        self.value = value

    def __call__(self):
        return self.value

def test_build_success_target_survives_tag_push_failure():
    state_path = os.path.join(tempfile.mkdtemp(), "state.json")
    repo = {"name": "repo-a", "full_name": "jjohnson83853/repo-a",
            "pushed_at": "t1", "clone_url": "https://github.com/x/repo-a.git"}

    class Controller:
        def record_target(self, state, name, generation):
            from graphify_refresh import record_target
            record_target(state, name, generation)

        def tick(self, state):
            pass

    events = []

    def fake_build(*args, on_built=None):
        events.append("built")
        on_built("b" * 40)
        events.append("push")
        raise app.BuildError(1, "push failed")

    orig = (app.list_owner_repos, app.has_dockerfile,
            app.get_latest_commit_sha, app.build_and_push)
    app.list_owner_repos = lambda *_: [repo]
    app.has_dockerfile = lambda *args: True
    app.get_latest_commit_sha = lambda *args: "a" * 40
    app.build_and_push = fake_build
    try:
        app.run_once("jjohnson83853", "tok", "main", "registry.local:5000",
                     state_path, graphify=Controller())
    finally:
        (app.list_owner_repos, app.has_dockerfile,
         app.get_latest_commit_sha, app.build_and_push) = orig
    saved = app.load_state(state_path)["repo-a"]
    assert events == ["built", "push"]
    assert saved["target"] == 1
    assert "sha" not in saved

def test_refresh_one_workspace_exec_snapshots_multiple_repos():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    record_target(state, "repo-b", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    assert len(client.creates) == 1 and len(client.starts) == 1
    attempt = state["graphify"]["attempt"]
    assert attempt["snapshot"] == {"repo-a": 1, "repo-b": 1}
    client.inspect_results.append((False, 0))
    controller.tick(state)
    assert state["repo-a"]["completed"] == state["repo-b"]["completed"] == 1
    assert "attempt" not in state["graphify"]


def test_active_workspace_exec_coalesces_repo_and_failure_marks_all_dirty():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    record_target(state, "repo-b", 1)
    client.inspect_results.append((False, 7))
    controller.tick(state)
    assert len(client.creates) == 1
    assert state["repo-a"]["failures"] == state["repo-b"]["failures"] == 1
    assert state["repo-a"]["retry_due"] == state["repo-b"]["retry_due"] == 1060


def test_refresh_success_only_acknowledges_saved_generation():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    record_target(state, "repo-a", 2)
    client.inspect_results.append((False, 0))
    controller.tick(state)
    assert state["repo-a"]["completed"] == 1
    assert state["repo-a"]["target"] == 2
    assert len(client.creates) == 1
    controller.tick(state)
    assert len(client.creates) == 2


def test_unresolved_inspect_blocks_other_repositories_and_inspects_once():
    import graphify_refresh as refresh
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    record_target(state, "repo-b", 1)
    events = []
    original_print = getattr(refresh, "print", None)

    def capture(*args, **kwargs):
        events.extend(args)

    class SecretError(Exception):
        status = 500

    client.inspect_results.append(SecretError("secret-response-body"))
    refresh.print = capture
    try:
        controller.tick(state)
    finally:
        if original_print is None:
            del refresh.print
        else:
            refresh.print = original_print
    assert len(client.inspects) == 1
    assert len(client.creates) == 1
    assert state["repo-b"]["target"] == 1
    assert "secret-response-body" not in str(events)


def test_nullable_inspect_and_restart_fallback_reuses_exec_id():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    eid = state["graphify"]["attempt"]["exec_id"]
    client.inspect_results.append((False, None))
    controller.tick(state)
    assert client.starts == [eid]
    assert len(client.creates) == 1
    clock.value += 60
    client.inspect_results.append((False, None))
    controller.tick(state)
    assert client.starts == [eid, eid]
    assert len(client.creates) == 1


def test_refresh_state_write_failure_suppresses_docker_actions():
    from graphify_refresh import record_target
    client = _RefreshClient()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, save=lambda _: False)
    controller.tick(state)
    assert client.creates == [] and client.starts == []


def test_refresh_event_sanitizes_repo_and_exec_identifiers():
    import graphify_refresh as refresh
    assert refresh.safe_repo("../token?") == "unknown"
    assert refresh.safe_exec_id("not-a-docker-id") is None
    events = []
    original_print = getattr(refresh, "print", None)
    refresh.print = lambda *args, **kwargs: events.extend(args)
    try:
        refresh.log("error", refresh.EXEC_FAILED, repos=["../token"],
                    status="secret-status", error_type="Secret error",
                    raw="secret-response")
    finally:
        if original_print is None:
            del refresh.print
        else:
            refresh.print = original_print
    line = str(events)
    assert "secret-status" not in line
    assert "Secret error" not in line
    assert "secret-response" not in line
    assert "unknown" in line
def test_crash_before_exec_create_waits_for_persisted_fallback():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    state["graphify"] = {"attempt": {
        "phase": "pre_create", "start_requested": False, "exec_id": None,
        "container_id": None, "fallback_deadline": 1060,
        "snapshot": {"repo-a": 1},
    }}
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    assert client.creates == []
    clock.value = 1060
    controller.tick(state)
    assert len(client.creates) == 1 and len(client.starts) == 1


def test_crash_fallback_uses_latest_snapshot_retry_delay():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    record_target(state, "repo-b", 1)
    state["repo-a"].update({"failures": 2, "retry_due": clock()})
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    assert state["graphify"]["attempt"]["fallback_deadline"] == clock() + 240


def test_definite_start_rejection_persists_failure_deadline():
    from graphify_refresh import DockerAPIError, record_target

    class RejectingClient(_RefreshClient):
        def exec_start(self, eid, on_status, cancel, transport):
            self.starts.append(eid)
            on_status(("rejected", 400))
            raise DockerAPIError(400)

    client, clock = RejectingClient(), _RefreshClock()
    client.inspect_results.append((False, None))
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    controller.tick(state)
    assert client.inspects == [client.starts[0]]
    assert state["repo-a"]["failures"] == 1
    assert state["repo-a"]["retry_due"] == 1060
    assert "attempt" not in state["graphify"]
    clock.value = 1059
    controller.tick(state)
    assert len(client.creates) == 1


def test_http_409_keeps_saved_exec_for_same_id_recovery():
    from graphify_refresh import DockerAPIError, record_target

    class AlreadyRunClient(_RefreshClient):
        def exec_start(self, eid, on_status, cancel, transport):
            self.starts.append(eid)
            on_status(("rejected", 409))
            raise DockerAPIError(409)

    client, clock = AlreadyRunClient(), _RefreshClock()
    client.inspect_results.append((False, None))
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)

    controller.tick(state)
    exec_id = state["graphify"]["attempt"]["exec_id"]
    controller.tick(state)

    assert state["graphify"]["attempt"]["exec_id"] == exec_id
    assert state["repo-a"].get("failures", 0) == 0
    assert client.creates == ["c" * 64]

    clock.value = 1060
    client.inspect_results.append((False, None))
    controller.tick(state)

    assert client.creates == ["c" * 64]
    assert client.starts == [exec_id, exec_id]


def test_ambiguous_start_is_inspected_without_replay():
    from graphify_refresh import DockerUnreachable, record_target

    class UncertainClient(_RefreshClient):
        def exec_start(self, eid, on_status, cancel, transport):
            self.starts.append(eid)
            on_status(("transport_error", None))
            raise DockerUnreachable()

    client, clock = UncertainClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    controller.tick(state)
    eid = state["graphify"]["attempt"]["exec_id"]
    client.inspect_results.append((True, None))
    controller.tick(state)
    assert client.inspects == [eid]
    assert client.creates == ["c" * 64]
    assert client.starts == [eid]


def test_exec_id_persistence_failure_suppresses_start():
    from graphify_refresh import record_target
    client, clock = _RefreshClient(), _RefreshClock()
    state = {}
    record_target(state, "repo-a", 1)
    saves = [0]

    def save(_state):
        saves[0] += 1
        return saves[0] != 2

    controller = _controller(client, now=clock, save=save)
    controller.tick(state)
    assert len(client.creates) == 1
    assert client.starts == []

def test_graphify_failure_logs_numeric_exit_code():
    from contextlib import redirect_stdout
    from io import StringIO
    import graphify_refresh as refresh

    client, clock = _RefreshClient(), _RefreshClock()
    client.inspect_results.append((False, 7))
    state = {}
    refresh.record_target(state, "repo-a", 1)
    controller = _controller(client, now=clock, save=lambda _: True)
    output = StringIO()
    with redirect_stdout(output):
        controller.tick(state)
        controller.tick(state)

    event = json.loads(output.getvalue().splitlines()[-1])
    assert event["outcome"] == "exec_failed"
    assert event["exit_code"] == 7


