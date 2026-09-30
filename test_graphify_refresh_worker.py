import json
import threading

import pytest

import graphify_stream

import graphify_refresh as refresh


class _Body:
    def read(self, _size, _cancel):
        return b""

    def close(self):
        pass


class _Transport:
    def __init__(self):
        self.cancelled = threading.Event()
        self.cancel_calls = 0

    def cancel(self):
        self.cancel_calls += 1
        self.cancelled.set()


class _BlockingClient:
    def __init__(self):
        self.inspection: tuple[bool, int | None] = (True, None)
        self.started = threading.Event()
        self.second_started = threading.Event()
        self.release = threading.Event()
        self.creates = []
        self.starts = []
    def container_id(self):
        return "c" * 64

    def exec_create(self, _container_id):
        exec_id = f"{len(self.creates) + 1:012x}"
        self.creates.append(exec_id)
        return exec_id

    def exec_start(self, exec_id, on_status, _cancel, _transport):
        self.starts.append(exec_id)
        if len(self.starts) == 2:
            self.second_started.set()
        on_status(("started", None))
        self.started.set()
        self.release.wait()
        return _Body()

    def exec_inspect(self, _exec_id):
        return self.inspection


@pytest.mark.parametrize(
    ("exit_code", "terminal_outcome"),
    [(0, refresh.EXEC_SUCCEEDED), (7, refresh.EXEC_FAILED)],
)
def test_terminal_inspect_guards_followup_until_capture_worker_exits(
        exit_code, terminal_outcome, capsys):
    client = _BlockingClient()
    transports = []
    clock = [100]
    graph = refresh.GraphifyRefresh(
        client, now=lambda: clock[0], save=lambda _state: True,
        attached_exec_factory=lambda _client: transports.append(_Transport())
        or transports[-1])
    state = {}
    refresh.record_target(state, "repo", 1)
    graph.tick(state)
    active_worker = graph.capture_worker

    try:
        assert client.started.wait(1)
        assert active_worker is not None
        client.inspection = (True, None)
        graph.tick(state)
        refresh.record_target(state, "repo", 2)
        client.inspection = (False, exit_code)
        graph.tick(state)

        assert graph._attempt(state) is None
        assert active_worker.is_alive()
        assert transports[0].cancelled.is_set()
        assert transports[0].cancel_calls == 1
        graph.tick(state)
        assert client.creates == ["000000000001"]
        assert client.starts == ["000000000001"]
        terminal_events = [
            json.loads(line) for line in capsys.readouterr().out.splitlines()
        ]
        terminal_events = [
            event for event in terminal_events
            if event.get("outcome") in {
                refresh.EXEC_SUCCEEDED, refresh.EXEC_FAILED,
            }
        ]
        assert [event["outcome"] for event in terminal_events] == [
            terminal_outcome,
        ]

        client.release.set()
        active_worker._thread.join(timeout=1)
        assert not active_worker.is_alive()
        if exit_code != 0:
            clock[0] = state["repo"]["retry_due"]
        graph.tick(state)
        assert client.second_started.wait(1)
        assert client.creates == ["000000000001", "000000000002"]
        assert client.starts == ["000000000001", "000000000002"]
        late_events = [
            json.loads(line) for line in capsys.readouterr().out.splitlines()
        ]
        assert not any(
            event.get("outcome") in {
                refresh.EXEC_SUCCEEDED, refresh.EXEC_FAILED,
            }
            for event in late_events)
    finally:
        client.release.set()
        if active_worker is not None:
            active_worker._thread.join(timeout=1)
        current_worker = graph.capture_worker
        if current_worker is not None and current_worker is not active_worker:
            current_worker._thread.join(timeout=1)


class _PendingJoinProcess:
    def __init__(self):
        self.alive = True
        self.terminated = threading.Event()
        self.join_entered = threading.Event()
        self.release_join = threading.Event()

    def is_alive(self):
        return self.alive

    def terminate(self):
        self.alive = False
        self.terminated.set()

    def join(self, timeout=0):
        self.join_entered.set()
        self.release_join.wait()


def test_terminal_inspect_does_not_wait_for_resolver_reaping():
    client = _BlockingClient()
    process = _PendingJoinProcess()
    transport = graphify_stream.AttachedExec("example:2375")
    start_finished = threading.Event()
    start_finished.set()
    transport._resolver = process
    transport._resolver_start_finished = start_finished
    transport._resolver_abandoned = threading.Event()
    graph = refresh.GraphifyRefresh(
        client, save=lambda _state: True,
        attached_exec_factory=lambda _client: transport)
    state = {}
    refresh.record_target(state, "repo", 1)
    graph.tick(state)
    assert client.started.wait(1)
    client.inspection = (False, 7)
    tick_returned = threading.Event()
    errors = []

    def inspect_terminal():
        try:
            graph.tick(state)
        except Exception as exc:
            errors.append(exc)
        finally:
            tick_returned.set()

    tick_thread = threading.Thread(target=inspect_terminal)
    tick_thread.start()
    try:
        assert tick_returned.wait(0.2), "terminal inspection blocked on resolver join"
        assert process.terminated.is_set()
        assert not process.join_entered.is_set()
        assert errors == []
    finally:
        process.release_join.set()
        client.release.set()
        tick_thread.join(timeout=1)
        worker = graph.capture_worker
        if worker is not None:
            worker._thread.join(timeout=1)

    assert not tick_thread.is_alive()
