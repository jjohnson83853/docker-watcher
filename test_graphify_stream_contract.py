import multiprocessing
import struct
import socket
import threading
import time

import pytest

import graphify_stream


class _Cancel:
    def is_set(self):
        return False


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

    def close(self):
        pass

class _HeaderStallSocket:
    def __init__(self):
        self.waiting = threading.Event()
        self.closed = threading.Event()

    def settimeout(self, _timeout):
        pass

    def connect(self, _address):
        pass

    def sendall(self, _request):
        pass

    def recv(self, _size):
        self.waiting.set()
        self.closed.wait()
        return b""

    def shutdown(self, _how):
        self.closed.set()

    def close(self):
        self.closed.set()


class _ResponseSocket:
    def __init__(self, response):
        self.response = bytearray(response)

    def settimeout(self, _timeout):
        pass

    def connect(self, _address):
        pass

    def sendall(self, _request):
        pass

    def recv(self, size):
        data = bytes(self.response[:size])
        del self.response[:size]
        return data

    def close(self):
        pass


class _CompletingProcess:
    def __init__(self):
        self.entered = threading.Event()
        self.release = threading.Event()
        self.returned = threading.Event()
        self.running = False
        self.terminated = threading.Event()
        self.killed = threading.Event()
        self.timed_joined = threading.Event()
        self.joined = threading.Event()
        self.terminated_while_slot_locked = False
        self.killed_while_slot_locked = False
        self.joined_while_slot_locked = False

    def start(self):
        self.entered.set()
        self.release.wait()
        self.running = True
        self.returned.set()

    def is_alive(self):
        return self.running

    def terminate(self):
        self.terminated_while_slot_locked = (
            graphify_stream._RESOLVER_START_SLOT.locked())
        self.terminated.set()

    def kill(self):
        self.killed_while_slot_locked = (
            graphify_stream._RESOLVER_START_SLOT.locked())
        self.running = False
        self.killed.set()

    def join(self, timeout=None):
        if timeout is None:
            self.joined_while_slot_locked = (
                graphify_stream._RESOLVER_START_SLOT.locked())
            self.joined.set()
        else:
            self.timed_joined.set()







def test_attached_exec_rejects_malformed_status_lines_before_started(
        monkeypatch):
    body = _frame()
    transport = graphify_stream.AttachedExec(
        "unused:1", unix_path="/docker.sock")
    statuses = []

    for status_line in (
            b"garbage 200 OK", b"HTTP/x 200 OK", b"HTTP/1.1 +200 OK",
            b"HTTP/1.1 20 OK", b"HTTP/1.1 200"):
        response = (status_line + b"\r\nContent-Length: "
                    + str(len(body)).encode() + b"\r\n\r\n" + body)
        monkeypatch.setattr(
            graphify_stream.socket, "socket",
            lambda *_args, response=response: _ResponseSocket(response))
        with pytest.raises(OSError):
            transport.start(
                "/exec/id/start", statuses.append, threading.Event())

    assert statuses == [("transport_error", None)] * 5


def test_resolver_start_timeout_cleans_child_if_start_finishes_at_deadline(
        monkeypatch):
    transport = graphify_stream.AttachedExec("example:2375", timeout=1)
    process = _CompletingProcess()
    real_monotonic = time.monotonic
    main_thread = threading.current_thread()
    deadline = real_monotonic() + 1
    helper_threads = []
    real_thread_start = threading.Thread.start

    def track_helper(thread, *args, **kwargs):
        if thread.name == "graphify-resolver-start":
            helper_threads.append(thread)
        return real_thread_start(thread, *args, **kwargs)

    def expire_after_helper_finishes():
        if threading.current_thread() is main_thread:
            assert process.entered.wait(1)
            process.release.set()
            assert process.returned.wait(1)
            assert helper_threads
            helper_threads[0].join(timeout=1)
            assert not helper_threads[0].is_alive()
            return deadline + 1
        return real_monotonic()

    monkeypatch.setattr(threading.Thread, "start", track_helper)
    monkeypatch.setattr(
        graphify_stream.time, "monotonic", expire_after_helper_finishes)

    with pytest.raises(TimeoutError):
        transport._start_resolver(process, threading.Event(), deadline)

    assert process.joined.is_set()
    assert process.terminated_while_slot_locked
    assert process.joined_while_slot_locked
    assert process.terminated.is_set()
    assert process.timed_joined.is_set()
    assert process.killed.is_set()
    assert process.joined.is_set()
    assert process.terminated_while_slot_locked
    assert process.killed_while_slot_locked
    assert process.joined_while_slot_locked


class _StalledProcess:
    def __init__(self):
        self.entered = threading.Event()
        self.release = threading.Event()
        self.terminated = threading.Event()
        self.running = False
        self.joins = []

    def start(self):
        self.entered.set()
        self.release.wait()
        self.running = True

    def is_alive(self):
        return self.running

    def terminate(self):
        self.running = False
        self.terminated.set()

    def join(self, timeout=0):
        self.joins.append(timeout)


def _frame(payload=b"ordinary diagnostic"):
    return b"\x02\x00\x00\x00" + struct.pack(">I", len(payload)) + payload

def test_resolver_child_exits_cleanly_after_receiver_closes(capfd):
    context = multiprocessing.get_context("spawn")
    receiver, sender = context.Pipe(duplex=False)
    receiver.close()
    process = context.Process(
        target=graphify_stream._resolve_child,
        args=("127.0.0.1", 80, sender),
        daemon=True)
    process.start()
    sender.close()
    process.join(timeout=2)
    if process.is_alive():
        process.terminate()
        process.join(timeout=1)

    captured = capfd.readouterr()
    assert process.exitcode == 0
    assert captured.err == ""


def _capture_result(headers, chunks):
    sock = _FragmentedSocket(chunks)
    worker = graphify_stream.CaptureWorker(
        lambda *_args: graphify_stream._SocketBody(sock, b"", headers))
    worker.start()
    worker._thread.join(timeout=1)
    assert not worker.is_alive()
    return worker.result()


def test_capture_rejects_content_length_truncated_after_complete_frame():
    frame = _frame()

    result = _capture_result(
        f"Content-Length: {len(frame) + 4}\r\n".encode(), [frame])

    assert result == "unavailable"


def test_capture_accepts_exact_content_length_boundary():
    frame = _frame()

    result = _capture_result(
        f"Content-Length: {len(frame)}\r\n".encode(), [frame])

    assert result == "unclassified"


def test_capture_rejects_zero_chunk_without_trailer_terminator():
    frame = _frame()
    wire = f"{len(frame):x}\r\n".encode() + frame + b"\r\n0\r\n"

    result = _capture_result(b"Transfer-Encoding: chunked\r\n", [wire])

    assert result == "unavailable"


def test_capture_accepts_complete_chunked_trailers():
    frame = _frame()
    wire = (f"{len(frame):x}\r\n".encode() + frame
            + b"\r\n0\r\nX-Trace: ignored\r\n\r\n")

    result = _capture_result(b"Transfer-Encoding: chunked\r\n", [wire])

    assert result == "unclassified"


@pytest.mark.parametrize("invalid_value", [b"\x00", b"\x1f", b"\x7f"])
def test_capture_rejects_control_characters_in_trailer_values(invalid_value):
    frame = _frame(b"error: path not found: /private")
    wire = (f"{len(frame):x}\r\n".encode() + frame
            + b"\r\n0\r\nX-Trace: " + invalid_value + b"\r\n\r\n")

    result = _capture_result(b"Transfer-Encoding: chunked\r\n", [wire])

    assert result == "unavailable"


class _TrailerTimeoutSocket(_FragmentedSocket):
    def __init__(self):
        super().__init__([b"0\r\n"])
        self.timed_out = False
        self.trailers = bytearray(b"\r\n")

    def recv(self, size):
        while self.chunks and not self.chunks[0]:
            self.chunks.pop(0)
        if self.chunks:
            return super().recv(size)
        if not self.timed_out:
            self.timed_out = True
            raise socket.timeout()
        data = bytes(self.trailers[:size])
        del self.trailers[:size]
        return data


def test_chunked_trailers_resume_after_idle_timeout():
    body = graphify_stream._SocketBody(
        _TrailerTimeoutSocket(), b"", b"Transfer-Encoding: chunked\r\n")

    with pytest.raises(socket.timeout):
        body.read(8, _Cancel())

    assert body.read(8, _Cancel()) == b""
    assert body.done


@pytest.mark.parametrize("chunk_size", [b"+0", b"0x0", b"0_0"])
def test_capture_rejects_non_http_chunk_size_syntax(chunk_size):
    frame = _frame()
    wire = (f"{len(frame):x}\r\n".encode() + frame + b"\r\n"
            + chunk_size + b"\r\n\r\n")

    result = _capture_result(b"Transfer-Encoding: chunked\r\n", [wire])

    assert result == "unavailable"



@pytest.mark.parametrize("headers", [
    b"invalid-field\r\n",
    b"Content-Length : 0\r\n",
    b"Transfer-Encoding: gzip\r\n",
    b"Content-Length:\r\n",
    b"Content-Length: 12x\r\n",
    b"Content-Length: " + b"9" * 5000 + b"\r\n",
    b"Content-Length: 0\r\nTransfer-Encoding: chunked\r\n",
    b"Content-Length: 0\r\nContent-Length: 0\r\n",
    b"Transfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n",
    b"HTTP/x 200 OK\r\nContent-Length: 0\r\n",
    b"X-Trace: invalid\x00\r\nContent-Length: 0\r\n",
])
def test_capture_rejects_ambiguous_or_invalid_http_body_framing(headers):
    assert _capture_result(headers, [b""]) == "unavailable"


def test_attached_exec_cancellation_closes_blocked_response_headers(monkeypatch):
    sock = _HeaderStallSocket()
    monkeypatch.setattr(
        graphify_stream.socket, "socket", lambda *_args: sock)
    transport = graphify_stream.AttachedExec(
        "unused:1", unix_path="/docker.sock", timeout=1)
    cancel = threading.Event()
    statuses = []
    errors = []

    def start():
        try:
            transport.start("/exec/id/start", statuses.append, cancel)
        except Exception as exc:
            errors.append(type(exc))

    worker = threading.Thread(target=start)
    worker.start()
    assert sock.waiting.wait(1)
    cancel.set()
    transport.cancel()
    worker.join(timeout=1)

    assert not worker.is_alive()
    assert sock.closed.is_set()
    assert len(errors) == 1
    assert statuses == [("transport_error", None)]


def test_resolver_startup_wait_is_cancellable_without_holding_transport_lock():
    transport = graphify_stream.AttachedExec("example:2375", timeout=1)
    process = _StalledProcess()
    cancel = threading.Event()
    outcome = []
    finished = threading.Event()

    def start_resolver():
        try:
            transport._start_resolver(
                process, cancel, time.monotonic() + 1)
        except Exception as exc:
            outcome.append(type(exc))
        finally:
            finished.set()

    starter = threading.Thread(target=start_resolver)
    starter.start()
    assert process.entered.wait(1)
    cancel.set()

    cancelled = threading.Event()
    cancel_thread = threading.Thread(
        target=lambda: (transport.cancel(), cancelled.set()))
    cancel_thread.start()
    try:
        assert cancelled.wait(0.2), "transport cancellation waited on process startup"
        assert finished.wait(0.2), "resolver startup did not observe cancellation"
        assert outcome == [InterruptedError]
    finally:
        process.release.set()
        starter.join(timeout=1)
        cancel_thread.join(timeout=1)

    assert not starter.is_alive()
    assert process.terminated.wait(1)


def test_resolver_process_startup_obeys_setup_deadline():
    transport = graphify_stream.AttachedExec("example:2375", timeout=0.03)
    process = _StalledProcess()
    outcome = []
    finished = threading.Event()

    def start_resolver():
        try:
            transport._start_resolver(
                process, threading.Event(), time.monotonic() + 0.03)
        except Exception as exc:
            outcome.append(type(exc))
        finally:
            finished.set()

    starter = threading.Thread(target=start_resolver)
    starter.start()
    assert process.entered.wait(1)
    try:
        assert finished.wait(0.3), "process startup exceeded its deadline"
        assert outcome == [TimeoutError]
    finally:
        process.release.set()
        starter.join(timeout=1)

    assert not starter.is_alive()
    assert process.terminated.wait(1)


def test_abandoned_resolver_start_blocks_retry_starter():
    transport = graphify_stream.AttachedExec("example:2375", timeout=0.03)
    process = _StalledProcess()
    outcome = []
    finished = threading.Event()

    def start_resolver():
        try:
            transport._start_resolver(
                process, threading.Event(), time.monotonic() + 0.03)
        except Exception as exc:
            outcome.append(type(exc))
        finally:
            finished.set()

    starter = threading.Thread(target=start_resolver)
    starter.start()
    assert process.entered.wait(1)

    retry_process = _StalledProcess()
    retry_transport = graphify_stream.AttachedExec("example:2375", timeout=1)
    try:
        assert finished.wait(0.3), "process startup exceeded its deadline"
        assert outcome == [TimeoutError]
        with pytest.raises(TimeoutError):
            retry_transport._start_resolver(
                retry_process, threading.Event(), time.monotonic() + 1)
        assert not retry_process.entered.is_set()
    finally:
        process.release.set()
        starter.join(timeout=1)

    assert not starter.is_alive()
    assert process.terminated.wait(1)


def test_resolver_process_start_error_cleans_registration():
    class FailedProcess:
        def __init__(self):
            self.joined = False

        def start(self):
            raise OSError

        def is_alive(self):
            return False

        def join(self, timeout=0):
            self.joined = True

    transport = graphify_stream.AttachedExec("example:2375", timeout=1)
    process = FailedProcess()

    with pytest.raises(OSError):
        transport._start_resolver(
            process, threading.Event(), time.monotonic() + 1)

    assert process.joined
    assert transport._resolver is None
