"""Regression contract for safe Graphify attached-stream observability."""
import json
import socket
import struct
import threading

import pytest

import app
import graphify_refresh as refresh
import graphify_stream


def _frame(stream_type, payload):
    return bytes([stream_type, 0, 0, 0]) + struct.pack(">I", len(payload)) + payload


def test_frame_parser_preserves_split_multiplexed_frames():
    parser = graphify_stream.FrameParser()
    wire = _frame(1, b"stdout error: path not found: ignored") + _frame(
        2, b"error: path not found: /private/workspace"
    )

    for byte in wire:
        parser.feed(bytes([byte]))

    assert parser.finish() == "workspace_path_missing"


def test_frame_parser_matches_only_stderr_and_prefers_path_signature():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(1, b"error: path not found: from stdout"))
    parser.feed(_frame(
        2,
        "Nothing to update or rebuild failed — check output above.\n"
        "error: path not found: /private/path".encode(),
    ))

    assert parser.finish() == "workspace_path_missing"


def test_frame_parser_classifies_exact_rebuild_signature_from_stderr():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(
        2, "Nothing to update or rebuild failed — check output above.".encode()
    ))

    assert parser.finish() == "graphify_rebuild_failed"


def test_frame_parser_marks_malformed_or_truncated_input_unavailable():
    malformed = graphify_stream.FrameParser()
    malformed.feed(bytes([3, 0, 0, 0]) + struct.pack(">I", 0))
    truncated = graphify_stream.FrameParser()
    truncated.feed(_frame(2, b"error: path not found: /partial")[:-1])

    assert malformed.finish() == "unavailable"
    assert truncated.finish() == "unavailable"


def test_frame_parser_rejects_declared_oversized_payload():
    parser = graphify_stream.FrameParser()
    parser.feed(bytes([2, 0, 0, 0]) + struct.pack(
        ">I", graphify_stream.MAX_FRAME_BYTES + 1
    ))

    assert parser.finish() == "unavailable"


def test_frame_parser_cap_counts_stdout_and_stderr_payload_bytes():
    parser = graphify_stream.FrameParser()
    cap = graphify_stream.SCAN_CAP_BYTES
    parser.feed(_frame(2, b"error: path not found: /seen\\n"))
    parser.feed(_frame(1, b"x" * (cap - len(b"error: path not found: /seen\\n"))))
    parser.feed(_frame(2, b"y"))

    assert parser.finish() == "unavailable"


class _Client:
    def __init__(self, inspect_result):
        self.inspect_result = inspect_result
        self.starts = []

    def exec_inspect(self, exec_id):
        return self.inspect_result

    def exec_start(self, exec_id, on_status, cancel, transport):
        self.starts.append(exec_id)

    def container_id(self):
        raise AssertionError("recovered exec must not create a replacement")

    def exec_create(self, container_id):
        raise AssertionError("recovered exec must not create a replacement")


def _saved_attempt():
    return {
        "phase": "started",
        "start_requested": True,
        "exec_id": "b" * 12,
        "container_id": "a" * 12,
        "fallback_deadline": 100,
        "snapshot": {"repo": 1},
    }


def test_terminal_nonzero_failure_emits_only_fixed_diagnostic_class(capsys):
    client = _Client((False, 7))
    state = {
        "repo": {"target": 1},
        refresh.RESERVED_KEY: {"attempt": _saved_attempt()},
    }

    refresh.GraphifyRefresh(client, now=lambda: 100).tick(state)

    events = [json.loads(line) for line in capsys.readouterr().out.splitlines()]
    failure = next(event for event in events if event["outcome"] == refresh.EXEC_FAILED)
    assert failure["diagnostic_class"] == "unavailable"
    assert set(failure["diagnostic_class"] for event in events
               if "diagnostic_class" in event) <= {
                   "workspace_path_missing", "graphify_rebuild_failed",
                   "unclassified", "unavailable",
               }


def test_recovered_running_exec_is_inspected_without_reattaching(capsys):
    client = _Client((True, None))
    state = {
        "repo": {"target": 1},
        refresh.RESERVED_KEY: {"attempt": _saved_attempt()},
    }

    refresh.GraphifyRefresh(client, now=lambda: 100).tick(state)

    assert client.starts == []
    assert state[refresh.RESERVED_KEY]["attempt"]["exec_id"] == "b" * 12
    events = [json.loads(line) for line in capsys.readouterr().out.splitlines()]
    assert any(event["outcome"] == refresh.EXEC_RUNNING for event in events)


class _FakeSocket:
    def __init__(self, response):
        self.response = response
        self.closed = False

    def settimeout(self, _timeout):
        pass

    def connect(self, _path):
        pass

    def sendall(self, _data):
        pass

    def recv(self, _size):
        response, self.response = self.response, b""
        return response

    def close(self):
        self.closed = True


class _Cancel:
    def is_set(self):
        return False


def test_attached_exec_rejection_releases_registered_socket(monkeypatch):
    sock = _FakeSocket(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec("localhost:2375", unix_path="/docker.sock")

    try:
        transport.start("/exec/id/start", lambda _status: None, _Cancel())
    except OSError:
        pass
    else:
        raise AssertionError("rejected attached start should fail")

    assert sock.closed
    assert transport._socket is None


def test_attached_exec_success_keeps_socket_until_stream_close(monkeypatch):
    sock = _FakeSocket(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec("localhost:2375", unix_path="/docker.sock")

    body = transport.start("/exec/id/start", lambda _status: None, _Cancel())

    assert transport._socket is sock
    assert not sock.closed
    body.close()
    assert sock.closed



class _Body:
    def __init__(self, chunks):
        self.chunks = list(chunks)
        self.read_sizes = []
        self.closed = False

    def read(self, size, _cancel):
        self.read_sizes.append(size)
        if not self.chunks:
            return b""
        item = self.chunks.pop(0)
        if isinstance(item, BaseException):
            raise item
        return item

    def close(self):
        self.closed = True


def _join_worker(worker):
    worker.start()
    worker._thread.join(timeout=2)
    assert not worker.is_alive()


def test_capture_worker_publishes_classification_after_normal_completion():
    body = _Body([_frame(2, b"error: path not found: /private")])
    worker = graphify_stream.CaptureWorker(
        lambda status, _cancel: (status(("started", None)) or body))

    _join_worker(worker)

    assert worker.status() == ("started", None)
    assert worker.result() == "workspace_path_missing"
    assert body.closed


def test_capture_worker_retries_idle_socket_timeout():
    body = _Body([socket.timeout(), _frame(2, b"")])
    worker = graphify_stream.CaptureWorker(lambda _status, _cancel: body)

    _join_worker(worker)

    assert len(body.read_sizes) == 3
    assert worker.result() == "unclassified"


def test_capture_worker_turns_read_exception_into_unavailable():
    body = _Body([RuntimeError("private socket detail")])
    worker = graphify_stream.CaptureWorker(lambda _status, _cancel: body)

    _join_worker(worker)

    assert worker.result() == "unavailable"
    assert body.closed


def test_capture_worker_cancellation_discards_partial_diagnostic():
    body = _Body([_frame(2, b"error: path not found: /private")])
    worker = graphify_stream.CaptureWorker(lambda _status, _cancel: body)
    worker._cancel.set()

    _join_worker(worker)

    assert worker.result() == "unavailable"
    assert body.closed


def test_capture_worker_drains_invalid_capture_to_eof():
    body = _Body([b"\x03\x00\x00\x00\x00\x00\x00\x00", b"drained", b""])
    worker = graphify_stream.CaptureWorker(lambda _status, _cancel: body)

    _join_worker(worker)

    assert body.chunks == []
    assert worker.result() == "unavailable"


def test_capture_worker_drains_capture_past_scan_cap_to_eof():
    cap = graphify_stream.SCAN_CAP_BYTES
    body = _Body([_frame(1, b"x" * (cap + 1)), b"drained", b""])
    worker = graphify_stream.CaptureWorker(lambda _status, _cancel: body)

    _join_worker(worker)

    assert body.chunks == []
    assert worker.result() == "unavailable"


def test_frame_parser_accepts_payload_exactly_at_scan_cap():
    parser = graphify_stream.FrameParser()
    parser.feed(_frame(1, b"x" * graphify_stream.SCAN_CAP_BYTES))

    assert parser.finish() == "unclassified"


def test_frame_parser_empty_stream_is_unclassified():
    assert graphify_stream.FrameParser().finish() == "unclassified"


class _ResolverPipe:
    def __init__(self, result=None, available=True, on_poll=None):
        self.result = result
        self.available = available
        self.on_poll = on_poll
        self.receiver_closed = False
        self.sender_closed = False

    def endpoints(self):
        return _ResolverReceiver(self), _ResolverSender(self)


class _ResolverReceiver:
    def __init__(self, pipe):
        self.pipe = pipe

    def poll(self, _timeout):
        if self.pipe.on_poll is not None:
            self.pipe.on_poll()
        return self.pipe.available

    def recv(self):
        return self.pipe.result

    def close(self):
        self.pipe.receiver_closed = True


class _ResolverSender:
    def __init__(self, pipe):
        self.pipe = pipe

    def close(self):
        self.pipe.sender_closed = True


class _ResolverProcess:
    def __init__(self, never_exits=False):
        self.running = False
        self.never_exits = never_exits
        self.terminated = False
        self.joins = []

    def start(self):
        self.running = True

    def is_alive(self):
        return self.running

    def join(self, timeout=0):
        self.joins.append(timeout)
        if not self.never_exits:
            self.running = False

    def terminate(self):
        self.terminated = True
        self.running = False


def _install_resolver_fakes(monkeypatch, pipe, process):
    class Context:
        def Pipe(self, duplex=False):
            return pipe.endpoints()

        def Process(self, **_kwargs):
            return process

    def get_context(method):
        assert method == "spawn"
        return Context()

    monkeypatch.setattr(graphify_stream.multiprocessing, "get_context",
                        get_context)


def test_resolve_child_sends_only_failure_marker(monkeypatch):
    sent = []

    class Sender:
        def send(self, value):
            sent.append(value)

        def close(self):
            sent.append("closed")

    def fail_lookup(*_args, **_kwargs):
        raise OSError("private resolver failure")

    monkeypatch.setattr(graphify_stream.socket, "getaddrinfo", fail_lookup)
    graphify_stream._resolve_child("docker.example", 2375, Sender())

    assert sent == [None, "closed"]


def test_bounded_resolver_returns_addresses_and_closes_pipe(monkeypatch):
    pipe = _ResolverPipe(result=[("address",)])
    process = _ResolverProcess()
    _install_resolver_fakes(monkeypatch, pipe, process)
    monkeypatch.setattr(graphify_stream.time, "monotonic", lambda: 0)

    result, child = graphify_stream._resolve_bounded(
        "docker.example", 2375, 10, _Cancel(), lambda resolver: resolver.start())

    assert result == [("address",)]
    assert child is process
    assert not process.is_alive()
    assert pipe.receiver_closed
    assert pipe.sender_closed


def test_bounded_resolver_cancellation_terminates_child(monkeypatch):
    cancelled = threading.Event()
    pipe = _ResolverPipe(available=False, on_poll=cancelled.set)
    process = _ResolverProcess()
    _install_resolver_fakes(monkeypatch, pipe, process)
    monkeypatch.setattr(graphify_stream.time, "monotonic", lambda: 0)

    with pytest.raises(InterruptedError):
        graphify_stream._resolve_bounded(
            "docker.example", 2375, 10, cancelled,
            lambda resolver: resolver.start())

    assert process.terminated
    assert pipe.receiver_closed
    assert pipe.sender_closed


def test_bounded_resolver_timeout_and_stuck_child_are_bounded(monkeypatch):
    cancelled = _Cancel()
    pipe = _ResolverPipe(available=False)
    process = _ResolverProcess()
    _install_resolver_fakes(monkeypatch, pipe, process)
    monkeypatch.setattr(graphify_stream.time, "monotonic", lambda: 0)

    with pytest.raises(TimeoutError):
        graphify_stream._resolve_bounded(
            "docker.example", 2375, -1, cancelled,
            lambda resolver: resolver.start())
    assert process.terminated

    pipe = _ResolverPipe(result=[("address",)])
    process = _ResolverProcess(never_exits=True)
    _install_resolver_fakes(monkeypatch, pipe, process)
    with pytest.raises(TimeoutError):
        graphify_stream._resolve_bounded(
            "docker.example", 2375, 10, cancelled,
            lambda resolver: resolver.start())
    assert process.terminated


def test_bounded_resolver_runs_from_worker_thread():
    completed = threading.Event()
    outcome = []

    def resolve():
        try:
            result = graphify_stream._resolve_bounded(
                "127.0.0.1", 2375,
                graphify_stream.time.monotonic() + 3,
                threading.Event(), lambda process: process.start())
            outcome.append(("ok", result))
        except Exception as exc:
            outcome.append(("error", exc))
        finally:
            completed.set()

    worker = threading.Thread(target=resolve)
    worker.start()
    try:
        assert completed.wait(5), "bounded resolver did not return"
    finally:
        worker.join(timeout=1)

    assert not worker.is_alive()
    assert outcome[0][0] == "ok", type(outcome[0][1]).__name__
    addresses, child = outcome[0][1]
    assert any(address[4][0] == "127.0.0.1" for address in addresses)
    child.close()


def test_tcp_attached_exec_resolves_hostname_and_preserves_host_header(monkeypatch):
    sock = _FakeSocket(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
    sock.requests = []
    sock.address = None
    sock.sendall = lambda request: sock.requests.append(request)
    sock.connect = lambda address: setattr(sock, "address", address)
    def resolve(host, port, _deadline, cancel, register):
        child = _ResolverProcess()
        register(child)
        return [
            (graphify_stream.socket.AF_INET,
             graphify_stream.socket.SOCK_STREAM, 6, "",
             ("192.0.2.8", port)),
        ], child

    monkeypatch.setattr(graphify_stream, "_resolve_bounded", resolve)
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    docker = refresh.DockerClient("tcp://docker-lxc.localdomain:2375")
    transport = graphify_stream.AttachedExec(docker._netloc, timeout=1)
    statuses = []

    body = transport.start("/exec/id/start", statuses.append, _Cancel())

    assert sock.address == ("192.0.2.8", 2375)
    assert b"Host: docker-lxc.localdomain:2375" in sock.requests[0]
    assert statuses == [("started", None)]
    body.close()


def test_attached_exec_reports_fragmented_response_header_status(monkeypatch):
    sock = _FakeSocket(b"")
    sock.responses = iter([b"HTTP/1.1 200 ", b"OK\r\nContent-Length: 0\r\n", b"\r\n"])
    sock.recv = lambda _size: next(sock.responses, b"")
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec("unused:1", unix_path="/docker.sock")
    statuses = []

    body = transport.start("/exec/id/start", statuses.append, _Cancel())

    assert statuses == [("started", None)]
    body.close()


def test_attached_exec_unix_request_uses_local_host_and_rejects_status(monkeypatch):
    sock = _FakeSocket(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
    sock.requests = []
    sock.sendall = lambda request: sock.requests.append(request)
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec("unused:1", unix_path="/docker.sock")
    statuses = []

    with pytest.raises(OSError):
        transport.start("/exec/id/start", statuses.append, _Cancel())

    assert b"Host: localhost" in sock.requests[0]
    assert statuses == [("rejected", 403)]
    assert sock.closed


class _FragmentedSocket:
    def __init__(self, chunks):
        self.chunks = [bytearray(chunk) for chunk in chunks]

    def recv(self, size):
        while self.chunks and not self.chunks[0]:
            self.chunks.pop(0)
        if not self.chunks:
            return b""
        chunk = self.chunks[0]
        data = bytes(chunk[:size])
        del chunk[:size]
        return data


def test_socket_body_decodes_fragmented_chunked_response():
    sock = _FragmentedSocket([
        b"4\r\nWiki\r\n", b"5;ext=x\r\npedia\r\n", b"0\r\n\r\n",
    ])
    body = graphify_stream._SocketBody(
        sock, b"", b"Transfer-Encoding: chunked\r\n")

    assert body.read(3, _Cancel()) == b"Wik"
    assert body.read(8, _Cancel()) == b"i"
    assert body.read(8, _Cancel()) == b"pedia"
    assert body.read(8, _Cancel()) == b""


def test_attached_exec_cancellation_before_start_reports_transport_error(monkeypatch):
    created = []
    monkeypatch.setattr(graphify_stream.socket, "socket",
                        lambda *_args: created.append(_FakeSocket(b"")) or created[-1])

    class Cancelled:
        def is_set(self):
            return True

    statuses = []
    transport = graphify_stream.AttachedExec("unused:1", unix_path="/docker.sock")
    with pytest.raises(InterruptedError):
        transport.start("/exec/id/start", statuses.append, Cancelled())

    assert statuses == [("transport_error", None)]
    assert created == []


def test_terminal_nonzero_logs_never_contain_raw_capture_output(capsys):
    secret = b"error: path not found: /private/customer/path"
    client = _Client((False, 9))
    state = {"repo": {"target": 1},
             refresh.RESERVED_KEY: {"attempt": _saved_attempt()}}
    body = _Body([_frame(2, secret)])
    worker = graphify_stream.CaptureWorker(
        lambda status, _cancel: (status(("started", None)) or body))
    _join_worker(worker)

    controller = refresh.GraphifyRefresh(client, now=lambda: 100)
    controller.capture_worker = worker
    controller.capture_exec_id = "b" * 12
    controller.tick(state)
    output = capsys.readouterr().out

    assert secret.decode() not in output
    assert b"/private" not in json.dumps(state).encode()
    assert "workspace_path_missing" in output


def test_recovered_terminal_exec_is_inspected_without_reattaching(capsys):
    client = _Client((False, 0))
    state = {"repo": {"target": 1},
             refresh.RESERVED_KEY: {"attempt": _saved_attempt()}}

    refresh.GraphifyRefresh(client, now=lambda: 100).tick(state)

    assert state[refresh.RESERVED_KEY].get("attempt") is None
    events = [json.loads(line) for line in capsys.readouterr().out.splitlines()]
    assert any(event["outcome"] == refresh.EXEC_SUCCEEDED for event in events)



def test_attached_exec_header_timeout_uses_bounded_remaining_time(monkeypatch):
    sock = _FakeSocket(b"")
    timeouts = []
    sock.settimeout = timeouts.append
    sock.recv = lambda _size: b"HTTP/1.1 200 "
    ticks = iter([0.0, 0.0, 0.1, 0.2, 1.1])
    monkeypatch.setattr(graphify_stream.time, "monotonic", lambda: next(ticks))
    monkeypatch.setattr(graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec("unused:1", unix_path="/docker.sock",
                                             timeout=1.0)

    with pytest.raises(TimeoutError):
        transport.start("/exec/id/start", lambda _status: None, _Cancel())

    assert timeouts
    assert max(timeouts) <= 1.0
    assert sock.closed


def test_watcher_build_push_continues_while_attached_stream_is_blocked(
        monkeypatch, tmp_path):
    calls = []
    started = threading.Event()
    release = threading.Event()
    done = threading.Event()
    errors = []
    monkeypatch.setattr(app, "list_owner_repos", lambda *_args: [{
        "name": "repo", "full_name": "owner/repo", "pushed_at": "new",
        "clone_url": "https://example.invalid/owner/repo.git",
    }])
    monkeypatch.setattr(app, "has_dockerfile", lambda *_args: True)
    monkeypatch.setattr(app, "get_latest_commit_sha", lambda *_args: "new-sha")

    def build_push(*_args, on_built=None):
        calls.append("build")
        on_built("new-sha")
        calls.extend(["push-latest", "push-sha"])
        return "new-sha"

    monkeypatch.setattr(app, "build_and_push", build_push)

    class Client:
        def container_id(self):
            calls.append("container")
            return "a" * 64

        def exec_create(self, _container_id):
            calls.append("create")
            return "b" * 12

        def exec_start(self, _exec_id, on_status, _cancel, _transport):
            calls.append("attached-start")
            on_status(("started", None))
            started.set()
            release.wait()
            return _Body([b""])

    class Transport:
        def cancel(self):
            pass

    state_path = tmp_path / "state.json"
    graphify = refresh.GraphifyRefresh(
        Client(), now=lambda: 100,
        save=lambda state: app.save_state(str(state_path), state),
        attached_exec_factory=lambda _client: Transport())

    def run_watcher():
        try:
            app.run_once("owner", "token", "main", "registry",
                         str(state_path), graphify=graphify)
        except Exception as exc:
            errors.append(exc)
        finally:
            done.set()

    watcher = threading.Thread(target=run_watcher)
    watcher.start()
    try:
        assert started.wait(2)
        assert done.wait(2), "watcher waited for the attached stream"
        assert graphify.capture_worker.is_alive()
        assert calls == [
            "build", "push-latest", "push-sha", "container", "create",
            "attached-start",
        ]
        assert json.loads(state_path.read_text())["repo"]["sha"] == "new-sha"
    finally:
        release.set()
        watcher.join(timeout=2)
        if graphify.capture_worker is not None:
            graphify.capture_worker._thread.join(timeout=2)

    assert not watcher.is_alive()
    assert not graphify.capture_worker.is_alive()
    assert errors == []


def test_attached_exec_registration_cancellation_releases_owned_resources():
    class Cancelled:
        def is_set(self):
            return True

    transport = graphify_stream.AttachedExec("unused:1", unix_path="/docker.sock")
    sock = _FakeSocket(b"")
    with pytest.raises(InterruptedError):
        transport._register("_socket", sock, Cancelled())
    assert sock.closed

    resolver = _ResolverProcess()
    resolver.start()
    sock = _FakeSocket(b"")
    sock.shutdown = lambda _how: (_ for _ in ()).throw(OSError)
    transport._socket = sock
    transport._resolver = resolver
    transport._resolver_start_finished = threading.Event()
    transport._resolver_start_finished.set()
    transport.cancel()
    assert sock.closed
    assert resolver.terminated


def test_capture_worker_start_failure_and_cancel_callback(monkeypatch):
    cancelled = []
    worker = graphify_stream.CaptureWorker(
        lambda _status, _cancel: None,
        cancel_callback=lambda: cancelled.append(True))

    def fail_start():
        raise RuntimeError("private thread failure")

    monkeypatch.setattr(worker._thread, "start", fail_start)
    worker.start()
    assert worker.status() == ("transport_error", None)
    assert worker.result() == "unavailable"

    worker.cancel()
    assert worker._cancel.is_set()
    assert cancelled == [True]


def test_capture_worker_close_failure_discards_classification():
    class FailingCloseBody:
        def read(self, _size, _cancel):
            return b""

        def close(self):
            raise OSError("private close failure")

    worker = graphify_stream.CaptureWorker(
        lambda status, _cancel: (
            status(("started", None)) or FailingCloseBody()))
    _join_worker(worker)

    assert worker.result() == "unavailable"


def test_socket_body_reads_pending_nonchunked_data_and_rejects_bad_chunks():
    sock = _FakeSocket(b"tail")
    body = graphify_stream._SocketBody(sock, b"abc", b"")
    assert body.read(2, _Cancel()) == b"ab"
    assert body.read(2, _Cancel()) == b"c"
    assert body.read(4, _Cancel()) == b"tail"

    malformed = graphify_stream._SocketBody(
        _FragmentedSocket([b"z\r\n"]), b"", b"Transfer-Encoding: chunked")
    with pytest.raises(OSError):
        malformed.read(1, _Cancel())

    truncated = graphify_stream._SocketBody(
        _FragmentedSocket([b"2\r\nx"]), b"", b"Transfer-Encoding: chunked")
    assert truncated.read(2, _Cancel()) == b"x"
    with pytest.raises(OSError):
        truncated.read(2, _Cancel())

    cancelled = type("Cancelled", (), {"is_set": lambda _self: True})()
    assert body.read(1, cancelled) == b""
