import json

import graphify_refresh as refresh


class _Body:
    def read(self, _size, _cancel):
        return b""

    def close(self):
        pass


class _Client:
    def __init__(self):
        self.inspections = 0

    def container_id(self):
        return "a" * 12

    def exec_create(self, _container_id):
        return "b" * 12

    def exec_start(self, _exec_id, on_status, _cancel, _transport):
        on_status(("started", None))
        return _Body()

    def exec_inspect(self, _exec_id):
        self.inspections += 1
        return False, 7



class _Transport:
    def cancel(self):
        pass


def test_failed_terminal_failure_save_retains_attempt_and_defers_event(capsys):
    client = _Client()
    fail_once = [True]

    def save(state):
        if state.get("repo", {}).get("failures") == 1 and fail_once[0]:
            fail_once[0] = False
            return False
        return True

    graph = refresh.GraphifyRefresh(
        client, now=lambda: 100, save=save,
        attached_exec_factory=lambda _client: _Transport())
    state = {"repo": {"target": 1}}
    graph.tick(state)
    exec_id = graph._attempt(state)["exec_id"]

    graph.tick(state)

    assert graph._attempt(state)["exec_id"] == exec_id
    assert state["repo"].get("failures", 0) == 0
    events = [json.loads(line)["outcome"]
              for line in capsys.readouterr().out.splitlines()]
    assert refresh.PERSIST_FAILURE in events
    assert refresh.EXEC_FAILED not in events

    graph.tick(state)

    assert graph._attempt(state) is None
    events = [json.loads(line)["outcome"]
              for line in capsys.readouterr().out.splitlines()]
    assert events.count(refresh.EXEC_FAILED) == 1
