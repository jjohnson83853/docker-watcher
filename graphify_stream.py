"""Bounded, privacy-preserving capture of Docker's non-TTY exec stream."""
import multiprocessing
import socket
import struct
import threading
import time

MAX_FRAME_BYTES = 1_048_576
SCAN_CAP_BYTES = 65_536
READ_SIZE = 8_192
SETUP_TIMEOUT_SECONDS = 10.0

# A delayed process start retains its daemon helper; a global slot prevents
# repeated retries from accumulating blocked startup threads.
_RESOLVER_START_SLOT = threading.Lock()
DIAGNOSTIC_CLASSES = frozenset({
    "workspace_path_missing", "graphify_rebuild_failed", "unclassified", "unavailable",
})
_PATH_SIGNATURE = b"error: path not found: "
_REBUILD_SIGNATURE = "Nothing to update or rebuild failed — check output above.".encode()


def classify_stderr(stderr):
    """Return only a fixed signature label; never return matched content."""
    lines = [line[:-1] if line.endswith(b"\r") else line
             for line in stderr.split(b"\n")]
    if any(line.startswith(_PATH_SIGNATURE) for line in lines):
        return "workspace_path_missing"
    if _REBUILD_SIGNATURE in lines:
        return "graphify_rebuild_failed"
    return "unclassified"


class FrameParser:
    """Incrementally decode Docker multiplexed frames within strict scan limits."""
    def __init__(self):
        self.header = bytearray()
        self.remaining = 0
        self.stream_type = 0
        self.payload_bytes = 0
        self.stderr = bytearray()
        self.invalid = False
        self.capped = False

    def feed(self, data):
        offset = 0
        while offset < len(data) and not self.invalid and not self.capped:
            if self.remaining == 0:
                need = 8 - len(self.header)
                take = min(need, len(data) - offset)
                self.header.extend(data[offset:offset + take])
                offset += take
                if len(self.header) != 8:
                    continue
                frame_type, reserved, length = self.header[0], self.header[1:4], struct.unpack(">I", self.header[4:])[0]
                self.header.clear()
                if frame_type not in (1, 2) or reserved != b"\x00\x00\x00" or length > MAX_FRAME_BYTES:
                    self.invalid = True
                    return
                self.stream_type, self.remaining = frame_type, length
                if length == 0:
                    continue
            take = min(self.remaining, len(data) - offset)
            self.payload_bytes += take
            if self.payload_bytes > SCAN_CAP_BYTES:
                self.capped = True
                return
            if self.stream_type == 2:
                self.stderr.extend(data[offset:offset + take])
            self.remaining -= take
            offset += take

    def finish(self):
        if self.invalid or self.capped or self.remaining or self.header:
            return "unavailable"
        return classify_stderr(self.stderr)



def _is_http_token(value):
    return bool(value) and all(
        48 <= byte <= 57 or 65 <= byte <= 90 or 97 <= byte <= 122
        or byte in b"!#$%&'*+-.^_`|~"
        for byte in value)


def _http_status_code(status_line):
    version, separator, remainder = status_line.partition(b" ")
    if not separator or version not in {b"HTTP/1.0", b"HTTP/1.1"}:
        raise OSError
    code, separator, reason = remainder.partition(b" ")
    if (not separator or len(code) != 3
            or any(byte < 48 or byte > 57 for byte in code)
            or any((byte < 32 and byte != 9) or byte == 127 for byte in reason)):
        raise OSError
    return int(code)
def _is_http_field_value(value):
    return all(
        byte == 9 or byte == 32 or 33 <= byte <= 126 or byte >= 128
        for byte in value)




def _body_framing(headers):
    transfer_encoding = None
    content_length = None
    lines = headers.split(b"\r\n")
    if lines and lines[0].startswith(b"HTTP/"):
        _http_status_code(lines[0])
        lines = lines[1:]
    for line in lines:
        if not line:
            continue
        name, separator, value = line.partition(b":")
        if (not separator or not _is_http_token(name)
                or not _is_http_field_value(value)):
            raise OSError
        name = name.lower()
        value = value.strip()
        if name == b"transfer-encoding":
            if transfer_encoding is not None or value.lower() != b"chunked":
                raise OSError
            transfer_encoding = b"chunked"
        elif name == b"content-length":
            if content_length is not None or not value:
                raise OSError
            if any(byte < 48 or byte > 57 for byte in value):
                raise OSError
            try:
                content_length = int(value)
            except ValueError:
                raise OSError
    if transfer_encoding is not None and content_length is not None:
        raise OSError
    return transfer_encoding is not None, content_length


def _terminate_resolver(process, wait=True):
    try:
        if not process.is_alive():
            if wait:
                process.join(timeout=0)
            return True
        process.terminate()
        if not wait:
            return False
        process.join(timeout=0.1)
        if process.is_alive():
            process.kill()
            process.join()
        return not process.is_alive()
    except AssertionError:
        return True
    except ValueError:
        return True
    except OSError:
        try:
            return not process.is_alive()
        except AssertionError:
            return True
        except (OSError, ValueError):
            return False


def _resolve_child(host, port, sender):
    try:
        try:
            addresses = socket.getaddrinfo(
                host, port, type=socket.SOCK_STREAM)
        except Exception:
            addresses = None
        try:
            sender.send(addresses)
        except Exception:
            pass
    finally:
        try:
            sender.close()
        except Exception:
            pass


def _resolve_bounded(host, port, deadline, cancel, register_resolver):
    # Use spawn: the watcher starts resolution from its stream worker thread,
    # and forking a multithreaded process can inherit locked runtime state.
    context = multiprocessing.get_context("spawn")
    receiver, sender = context.Pipe(duplex=False)
    process = context.Process(
        target=_resolve_child, args=(host, port, sender), daemon=True)
    started = False
    try:
        register_resolver(process)
        started = True
        sender.close()
        while not cancel.is_set():
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not receiver.poll(min(remaining, 0.05)):
                if remaining <= 0:
                    raise TimeoutError
                continue
            result = receiver.recv()
            if result is None:
                raise OSError
            process.join(timeout=max(0, deadline - time.monotonic()))
            if process.is_alive():
                raise TimeoutError
            if cancel.is_set():
                raise InterruptedError
            return result, process
        raise InterruptedError
    except BaseException:
        if started:
            while not _terminate_resolver(process):
                time.sleep(0.05)
        raise
    finally:
        sender.close()
        receiver.close()


class AttachedExec:
    """Per-start socket cancellation and bounded Docker HTTP setup."""
    def __init__(self, host, unix_path=None, timeout=SETUP_TIMEOUT_SECONDS):
        self.host = host
        self.unix_path = unix_path
        self.timeout = timeout
        self._lock = threading.Lock()
        self._socket = None
        self._resolver = None
        self._resolver_start_finished = None
        self._resolver_abandoned = None

    def cancel(self):
        with self._lock:
            resolver, sock = self._resolver, self._socket
            start_finished = self._resolver_start_finished
            abandoned = self._resolver_abandoned
            self._resolver = self._socket = None
            self._resolver_start_finished = self._resolver_abandoned = None
        if abandoned is not None:
            abandoned.set()
        if (resolver is not None
                and (start_finished is None or start_finished.is_set())):
            _terminate_resolver(resolver, wait=False)
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            sock.close()

    def _register(self, field, resource, cancel):
        with self._lock:
            if cancel.is_set():
                resource.close()
                raise InterruptedError
            setattr(self, field, resource)

    def _start_resolver(self, process, cancel, deadline):
        start_finished = threading.Event()
        abandoned = threading.Event()
        start_error = []
        startup_lock = threading.Lock()
        cleanup_claimed = False
        with self._lock:
            if cancel.is_set():
                raise InterruptedError
            if not _RESOLVER_START_SLOT.acquire(blocking=False):
                raise TimeoutError
            self._resolver = process
            self._resolver_start_finished = start_finished
            self._resolver_abandoned = abandoned

        release_lock = threading.Lock()
        slot_released = False

        def release_slot():
            nonlocal slot_released
            with release_lock:
                if not slot_released:
                    slot_released = True
                    _RESOLVER_START_SLOT.release()

        def clear_registration():
            with self._lock:
                if self._resolver is process:
                    self._resolver = None
                    self._resolver_start_finished = None
                    self._resolver_abandoned = None

        def cleanup_resolver():
            while not _terminate_resolver(process):
                time.sleep(0.05)
            clear_registration()
            release_slot()

        def start_process():
            nonlocal cleanup_claimed
            try:
                if not abandoned.is_set() and not cancel.is_set():
                    process.start()
            except BaseException as exc:
                with startup_lock:
                    start_error.append(exc)
            finally:
                with startup_lock:
                    start_finished.set()
                    cleanup = (
                        (abandoned.is_set() or cancel.is_set())
                        and not cleanup_claimed
                    )
                    if cleanup:
                        cleanup_claimed = True
                if cleanup:
                    cleanup_resolver()

        starter = threading.Thread(
            target=start_process, name="graphify-resolver-start", daemon=True)
        try:
            starter.start()
        except BaseException:
            with startup_lock:
                abandoned.set()
                cleanup_claimed = True
            release_slot()
            clear_registration()
            raise

        while True:
            remaining = deadline - time.monotonic()
            cleanup = False
            failure = None
            wait_for = None
            with startup_lock:
                if start_finished.is_set():
                    if start_error:
                        failure = start_error[0]
                    elif cancel.is_set() or abandoned.is_set():
                        failure = InterruptedError()
                    else:
                        remaining = deadline - time.monotonic()
                        if remaining > 0:
                            release_slot()
                            return
                        failure = TimeoutError()
                    abandoned.set()
                    if not cleanup_claimed:
                        cleanup_claimed = True
                        cleanup = True
                elif cancel.is_set():
                    abandoned.set()
                    failure = InterruptedError()
                elif remaining <= 0:
                    abandoned.set()
                    failure = TimeoutError()
                else:
                    wait_for = min(remaining, 0.05)
            if cleanup:
                cleanup_resolver()
            if failure is not None:
                raise failure
            start_finished.wait(wait_for)
    def start(self, request_path, on_status, cancel):
        deadline = time.monotonic() + self.timeout
        resolver = None
        sock = None
        returned_body = False
        status_reported = False
        try:
            if cancel.is_set():
                raise InterruptedError
            if self.unix_path is not None:
                sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                self._register("_socket", sock, cancel)
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError
                sock.settimeout(remaining)
                if cancel.is_set():
                    raise InterruptedError
                sock.connect(self.unix_path)
            else:
                if self.host.startswith("["):
                    boundary = self.host.find("]")
                    host = self.host[1:boundary]
                    port_text = self.host[boundary + 2:]
                else:
                    host, port_text = self.host.rsplit(":", 1)
                addresses, resolver = _resolve_bounded(
                    host, int(port_text), deadline, cancel,
                    lambda child: self._start_resolver(
                        child, cancel, deadline))
                with self._lock:
                    self._resolver = None
                    self._resolver_start_finished = None
                    self._resolver_abandoned = None
                sock = None
                for family, socktype, proto, _, address in addresses:
                    if cancel.is_set() or time.monotonic() >= deadline:
                        raise TimeoutError
                    candidate = socket.socket(family, socktype, proto)
                    self._register("_socket", candidate, cancel)
                    if cancel.is_set():
                        raise InterruptedError
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise TimeoutError
                    candidate.settimeout(remaining)
                    try:
                        candidate.connect(address)
                        sock = candidate
                        break
                    except OSError:
                        candidate.close()
                        with self._lock:
                            if self._socket is candidate:
                                self._socket = None
                if sock is None:
                    raise OSError
            if cancel.is_set():
                raise InterruptedError
            body = b'{"Detach":false,"Tty":false}'
            host_header = self.host if self.unix_path is None else "localhost"
            request = (f"POST {request_path} HTTP/1.1\r\nHost: {host_header}\r\n"
                       "Content-Type: application/json\r\nConnection: close\r\n"
                       f"Content-Length: {len(body)}\r\n\r\n").encode() + body
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError
            sock.settimeout(remaining)
            if cancel.is_set():
                raise InterruptedError
            sock.sendall(request)
            header = bytearray()
            delimiter = b"\r\n\r\n"
            while True:
                if cancel.is_set():
                    raise InterruptedError
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError
                sock.settimeout(remaining)
                block = sock.recv(READ_SIZE)
                if not block:
                    raise OSError
                header.extend(block)
                separator = header.find(delimiter)
                if separator >= 0:
                    if separator > 65_536:
                        raise OSError
                    response_headers = bytes(header[:separator])
                    initial_body = bytes(header[separator + len(delimiter):])
                    break
                if len(header) > 65_536:
                    raise OSError
            status = _http_status_code(
                response_headers.split(b"\r\n", 1)[0])
            if not 200 <= status < 300:
                on_status(("rejected", status))
                status_reported = True
                raise OSError
            on_status(("started", None))
            sock.settimeout(self.timeout)
            body = _SocketBody(sock, initial_body, response_headers)
            returned_body = True
            return body
        except (OSError, TimeoutError, InterruptedError, ValueError):
            if not status_reported:
                on_status(("transport_error", None))
            raise
        finally:
            if not returned_body:
                with self._lock:
                    owned_socket, self._socket = self._socket, None
                if owned_socket is not None:
                    owned_socket.close()
            if resolver is not None and resolver.is_alive():
                resolver.join(timeout=0)


class _SocketBody:
    """Fixed-buffer body reader with HTTP transfer framing."""
    def __init__(self, sock, initial, headers):
        self.sock = sock
        self.pending = bytearray(initial)
        self.chunked, self.content_remaining = _body_framing(headers)
        self.chunk_remaining = None
        self.chunk_header = bytearray()
        self.chunk_delimiter = 0
        self.reading_trailers = False
        self.trailer_line = bytearray()
        self.trailer_bytes = 0
        self.done = False

    def _read_some(self, size):
        if self.pending:
            take = min(size, len(self.pending))
            data = bytes(self.pending[:take])
            del self.pending[:take]
            return data
        return self.sock.recv(size)

    def _read_chunk_header(self, cancel):
        while not cancel.is_set():
            part = self._read_some(1)
            if not part or len(self.chunk_header) >= 128:
                raise OSError
            self.chunk_header.extend(part)
            if self.chunk_header.endswith(b"\r\n"):
                chunk_line = bytes(self.chunk_header[:-2])
                size_text = chunk_line.partition(b";")[0]
                if (not size_text
                        or any(byte not in b"0123456789abcdefABCDEF"
                               for byte in size_text)):
                    raise OSError
                size = int(size_text, 16)
                self.chunk_header.clear()
                return size
        return None

    def _read_trailers(self, cancel):
        while not cancel.is_set():
            part = self._read_some(1)
            if not part:
                raise OSError
            self.trailer_bytes += 1
            if self.trailer_bytes > 65_536:
                raise OSError
            self.trailer_line.extend(part)
            if self.trailer_line.endswith(b"\r\n"):
                field = bytes(self.trailer_line[:-2])
                if not field:
                    self.trailer_line.clear()
                    self.reading_trailers = False
                    return True
                name, separator, value = field.partition(b":")
                if (not separator or not _is_http_token(name)
                        or not _is_http_field_value(value)
                        or name.lower() in {
                            b"content-length", b"transfer-encoding",
                        }):
                    raise OSError
                self.trailer_line.clear()
        return False

    def read(self, size, cancel):
        while not cancel.is_set():
            if self.done:
                return b""
            if not self.chunked:
                if self.content_remaining is None:
                    return self._read_some(size)
                if self.content_remaining == 0:
                    self.done = True
                    return b""
                data = self._read_some(min(size, self.content_remaining))
                if not data:
                    raise OSError
                self.content_remaining -= len(data)
                return data
            if self.chunk_delimiter:
                expected = b"\r\n"[2 - self.chunk_delimiter:]
                part = self._read_some(self.chunk_delimiter)
                if not part or not expected.startswith(part):
                    raise OSError
                self.chunk_delimiter -= len(part)
                if self.chunk_delimiter:
                    continue
                self.chunk_remaining = None
            if self.reading_trailers:
                if not self._read_trailers(cancel):
                    return b""
                self.done = True
                return b""
            if self.chunk_remaining is None:
                self.chunk_remaining = self._read_chunk_header(cancel)
                if self.chunk_remaining is None:
                    return b""
                if self.chunk_remaining == 0:
                    self.reading_trailers = True
                    continue
            data = self._read_some(min(size, self.chunk_remaining))
            if not data:
                raise OSError
            self.chunk_remaining -= len(data)
            if self.chunk_remaining == 0:
                self.chunk_delimiter = 2
            return data
        return b""

    def close(self):
        self.sock.close()

class CaptureWorker:
    """Dedicated daemon thread with one nonblocking terminal result handoff."""
    def __init__(self, start, cancel_callback=None):
        self._start = start
        self._cancel_callback = cancel_callback
        self._cancel = threading.Event()
        self._lock = threading.Lock()
        self._start_status = ("pending", None)
        self._result = None
        self._thread = threading.Thread(
            target=self._run, name="graphify-capture", daemon=True)

    def start(self):
        try:
            self._thread.start()
        except Exception:
            with self._lock:
                self._start_status = ("transport_error", None)
                self._result = "unavailable"

    def _publish_status(self, status):
        with self._lock:
            self._start_status = status

    def _run(self):
        parser = FrameParser()
        result = "unavailable"
        body = None
        try:
            body = self._start(self._publish_status, self._cancel)
            while not self._cancel.is_set():
                try:
                    chunk = body.read(READ_SIZE, self._cancel)
                except socket.timeout:
                    continue
                if not chunk:
                    if not self._cancel.is_set():
                        result = parser.finish()
                    break
                parser.feed(chunk)
                if parser.invalid or parser.capped:
                    while not self._cancel.is_set():
                        try:
                            if not body.read(READ_SIZE, self._cancel):
                                break
                        except socket.timeout:
                            continue
                    break
        except Exception:
            result = "unavailable"
        finally:
            if body is not None:
                close = getattr(body, "close", None)
                if close is not None:
                    try:
                        close()
                    except Exception:
                        result = "unavailable"
            with self._lock:
                self._result = result

    def status(self):
        with self._lock:
            return self._start_status

    def result(self):
        with self._lock:
            return self._result

    def is_alive(self):
        return self._thread.is_alive()

    def cancel(self):
        self._cancel.set()
        if self._cancel_callback is not None:
            self._cancel_callback()
