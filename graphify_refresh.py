"""Graphify workspace refresh for docker-watcher.

After a successful image build the watcher persists a durable per-repository
target generation (the built SHA) and drives ONE globally serialized detached
Docker Engine exec in the fixed `graphify-mcp` container running the fixed
argv `graphify update /workspace` with `GRAPHIFY_OUT=/data`.

Verified deployment contract (`.superpowers/sdd/graphify-refresh/design.md`):
- Route: the watcher's existing DOCKER_HOST (socket-proxy), Engine API
  negotiated via bounded GET /version; TCP and unix:// endpoints supported.
- Lifecycle: exec create -> persist exec ID -> persist start_requested ->
  detached start (Detach=true, Tty=false) -> ExecInspect with a nullable
  ExitCode (never coerced to zero).
- Recovery matrix: Running=true -> retain/poll; non-null ExitCode -> terminal
  (0 success, nonzero failure); Running=false, ExitCode=null -> not started,
  the saved exec may be started again only after a persisted deadline;
  missing/uninspectable ID or a replaced Graphify container stays unresolved
  and blocks all repositories until the same ID resolves.
- Serialization: at most one watcher-owned exec may be active or unresolved
  globally; it counts as in-flight for every repository. Graphify's shared
  /data/.rebuild.lock is additional defense only.

Retry clocks are per repository: initial 60 seconds, doubled per observed
failure, capped at 30 minutes. Coalesced builds never reset a clock.
"""
import http.client
import json
import re
import socket
import time

GRAPHIFY_CONTAINER = "graphify-mcp"
GRAPHIFY_ARGV = ["graphify", "update", "/workspace"]
GRAPHIFY_ENV = ["GRAPHIFY_OUT=/data"]
RESERVED_KEY = "graphify"

RETRY_BASE_SECONDS = 60
RETRY_CAP_SECONDS = 1800

# Fixed internal outcome vocabulary (design.md section 4). These are the only
# values the `outcome` field may carry; nothing external or unvalidated may
# be rendered there.
EXEC_CREATE_ERROR = "exec_create_error"
EXEC_START_UNCONFIRMED = "exec_start_unconfirmed"
EXEC_RUNNING = "exec_running"
EXEC_FAILED = "exec_failed"
EXEC_SUCCEEDED = "exec_succeeded"
EXEC_INSPECT_ERROR = "exec_inspect_error"
UNRESOLVED_EXEC = "unresolved_exec"
PERSIST_FAILURE = "persist_failure"
EXEC_STARTED = "exec_started"
RETRY_SCHEDULED = "retry_scheduled"
_OUTCOMES = {EXEC_CREATE_ERROR, EXEC_START_UNCONFIRMED, EXEC_RUNNING,
             EXEC_FAILED, EXEC_SUCCEEDED, EXEC_INSPECT_ERROR,
             UNRESOLVED_EXEC, PERSIST_FAILURE, EXEC_STARTED, RETRY_SCHEDULED}
_ERROR_TYPE_RE = re.compile(r"^[A-Za-z][A-Za-z0-9_]{0,79}$")

_REPO_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$")
_ID_RE = re.compile(r"^[0-9a-f]{12,64}$")


def log(level, outcome, **fields):
    """Emit only bounded internal fields; never render response/error text."""
    record = {
        "level": level if level in ("info", "warning", "error") else "error",
        "event": "graphify_refresh",
        "outcome": outcome if outcome in _OUTCOMES else "unresolved_exec",
    }
    for key, value in fields.items():
        if key == "repo":
            record[key] = safe_repo(value)
        elif key == "repos" and isinstance(value, (list, tuple)):
            record[key] = [safe_repo(repo) for repo in value[:100]]
        elif key == "exec_id":
            safe_id = safe_exec_id(value)
            if safe_id:
                record[key] = safe_id
        elif key == "status":
            record[key] = safe_status(value)
        elif key == "error_type":
            record[key] = value if isinstance(value, str) and _ERROR_TYPE_RE.match(value) else "unknown"
        elif key == "reason" and value in ("invalid_saved_id", "invalid_config"):
            record[key] = value
        elif key == "not_persisted" and isinstance(value, bool):
            record[key] = value
        elif key == "retry_delay" and type(value) is int:
            record[key] = max(0, min(value, RETRY_CAP_SECONDS))
        elif key == "exit_code":
            record[key] = safe_exit_code(value)
    print(json.dumps(record), flush=True)


def safe_repo(name):
    """Canonical bounded repo identifier, else a fixed safe value (NFR-3)."""
    if isinstance(name, str) and _REPO_RE.match(name):
        return name
    return "unknown"


def safe_exec_id(exec_id):
    """Exec IDs are logged only after strict bounded opaque-token validation;
    anything else is omitted (NFR-3)."""
    if isinstance(exec_id, str) and _ID_RE.match(exec_id):
        return exec_id
    return None


def safe_status(status):
    """Docker Engine HTTP status: an integer from 100-599 or `unknown`."""
    if isinstance(status, int) and 100 <= status <= 599:
        return status
    return "unknown"

def safe_exit_code(exit_code):
    """Docker exec process exit status: an integer from 0-255 or `unknown`."""
    if type(exit_code) is int and 0 <= exit_code <= 255:
        return exit_code
    return "unknown"


def retry_delay(failures):
    """Delay after an observed failure, capped at 30 minutes."""
    f = max(1, int(failures or 0))
    if f >= 6:
        return RETRY_CAP_SECONDS
    return RETRY_BASE_SECONDS * (2 ** (f - 1))


# ---------------------------------------------------------------------------
# Docker Engine client (stdlib only, bounded per-request timeout)
# ---------------------------------------------------------------------------

class _UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self._unix_path = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self._unix_path)
        self.sock = s


class DockerAPIError(Exception):
    """The Engine answered with a non-2xx status."""

    def __init__(self, status):
        self.status = safe_status(status)
        super().__init__("docker api error")


class DockerUnreachable(Exception):
    """Connection/timeout failure: no HTTP status was ever received."""


class DockerClient:
    """Minimal Engine API client: version negotiation, exec create/start/inspect."""

    def __init__(self, docker_host, request_timeout=10.0,
                 container=GRAPHIFY_CONTAINER, argv=GRAPHIFY_ARGV, env=GRAPHIFY_ENV):
        self.request_timeout = request_timeout
        self.container = container
        self.argv = list(argv)
        self.env = list(env)
        self._unix_path = None
        self._netloc = None
        host = (docker_host or "").strip()
        if host.startswith("unix://"):
            self._unix_path = host[len("unix://"):].split("?")[0]
            if not self._unix_path:
                raise ValueError("empty unix socket path")
        elif host.startswith("tcp://"):
            self._netloc = host[len("tcp://"):].split("/")[0]
            if not self._netloc:
                raise ValueError("empty tcp endpoint")
        else:
            raise ValueError("unsupported DOCKER_HOST scheme")
        self._api_prefix = None  # negotiated once via GET /version

    def request(self, method, path, body=None):
        """One bounded Engine API request. Returns (status, parsed-data).
        Raises DockerAPIError on non-2xx and DockerUnreachable on
        connection/timeout failures. Never logs raw responses."""
        if self._unix_path is not None:
            conn = _UnixHTTPConnection(self._unix_path, timeout=self.request_timeout)
        else:
            conn = http.client.HTTPConnection(self._netloc, timeout=self.request_timeout)
        try:
            payload = None
            if body is not None:
                payload = json.dumps(body)
            headers = {"Content-Type": "application/json"}
            try:
                conn.request(method, path, body=payload, headers=headers)
                resp = conn.getresponse()
                status = resp.status
                raw = resp.read()
            except (OSError, socket.timeout, http.client.HTTPException):
                raise DockerUnreachable()
            if not (200 <= status < 300):
                raise DockerAPIError(status)
            try:
                data = json.loads(raw) if raw else {}
            except ValueError:
                data = {}
            return status, data
        finally:
            conn.close()

    def api_version(self):
        _, data = self.request("GET", "/version")
        version = data.get("ApiVersion") if isinstance(data, dict) else None
        if not isinstance(version, str) or not re.match(r"^\d+\.\d+$", version):
            raise DockerAPIError("unknown")
        return version

    def _prefix(self):
        if self._api_prefix is None:
            self._api_prefix = f"/v{self.api_version()}"
        return self._api_prefix

    def container_id(self, name=None):
        status, data = self.request(
            "GET", f"{self._prefix()}/containers/{name or self.container}/json")
        cid = data.get("Id") if isinstance(data, dict) else None
        if not isinstance(cid, str) or not _ID_RE.match(cid):
            raise DockerAPIError(status)
        return cid
    def exec_create(self, container_id):
        body = {
            "AttachStdin": False, "AttachStdout": False, "AttachStderr": False,
            "Tty": False, "Cmd": self.argv, "Env": self.env,
        }
        status, data = self.request(
            "POST", f"{self._prefix()}/containers/{container_id}/exec", body)
        eid = data.get("Id") if isinstance(data, dict) else None
        if not isinstance(eid, str) or not _ID_RE.match(eid):
            raise DockerAPIError(status)
        return eid

    def exec_start(self, exec_id):
        # Detached start: Engine returns 200 with an empty body.
        self.request("POST", f"{self._prefix()}/exec/{exec_id}/start",
                     body={"Detach": True, "Tty": False})

    def exec_inspect(self, exec_id):
        _, data = self.request("GET", f"{self._prefix()}/exec/{exec_id}/json")
        if not isinstance(data, dict) or "ExitCode" not in data:
            raise DockerAPIError(200)
        running = data.get("Running")
        exit_code = data.get("ExitCode")
        if type(running) is not bool or (exit_code is not None
                                         and type(exit_code) is not int):
            raise DockerAPIError(200)
        return running, exit_code


# ---------------------------------------------------------------------------
# State access
# ---------------------------------------------------------------------------

def _entry(state, name):
    v = state.get(name)
    if isinstance(v, str):
        # v1 bare-string shape (ADR-7): promote to a dict on first write.
        v = {"sha": v}
        state[name] = v
    elif not isinstance(v, dict):
        v = {}
        state[name] = v
    return v


def ensure_entry(state, name):
    """Returns the repo entry, creating it only in-memory; persistence is the
    caller's responsibility via its save callback."""
    return _entry(state, name)


def record_target(state, name, generation):
    """FR-1: one durable refresh intent per successful build generation."""
    _entry(state, name)["target"] = generation


def target(state, name):
    v = state.get(name)
    if isinstance(v, dict):
        return v.get("target")
    return None


def completed(state, name):
    v = state.get(name)
    if isinstance(v, dict):
        return v.get("completed")
    return None


def failures(state, name):
    v = state.get(name)
    if isinstance(v, dict):
        return int(v.get("failures") or 0)
    return 0


def retry_due(state, name):
    v = state.get(name)
    if isinstance(v, dict):
        return v.get("retry_due")
    return None


def dirty_repos(state):
    """Repos with a durable target not yet marked completed."""
    out = []
    for name, v in state.items():
        if name == RESERVED_KEY or not isinstance(v, dict):
            continue
        t, c = v.get("target"), v.get("completed")
        if t and t != c:
            out.append(name)
    return sorted(out)


def eligible_repos(state, now):
    """Dirty repos with no retry deadline or one already due."""
    out = []
    for r in dirty_repos(state):
        due = retry_due(state, r)
        if due is None or now >= due:
            out.append(r)
    return out


def _clear_retry(state, name):
    v = _entry(state, name)
    v.pop("failures", None)
    v.pop("retry_due", None)


def _acknowledge(state, repo, generation):
    v = _entry(state, repo)
    v["completed"] = generation
    _clear_retry(state, repo)


# ---------------------------------------------------------------------------
# Lifecycle controller
# ---------------------------------------------------------------------------

class GraphifyRefresh:
    """Drives the serialized workspace exec from the existing watcher state.

    `save` is the watcher's atomic persist (returns True on success); every
    transition that enables an external action is persisted before acting,
    and a failed save suppresses the dependent action (NFR-2).
    """

    def __init__(self, client, now=time.time, save=None):
        self.client = client
        self.now = now
        self.save = save if save is not None else (lambda state: True)

    # -- entry points used by app.run_once ----------------------------------

    def record_target(self, state, name, generation):
        """Persist the per-repository target generation; the caller saves."""
        _entry(state, name)["target"] = generation

    def tick(self, state):
        """One watcher-cycle step: at most one create, one start and one
        inspect. Never raises for Graphify/Docker problems."""
        try:
            self._tick(state)
        except Exception as exc:  # NFR-1 isolation; never log raw text
            log("error", UNRESOLVED_EXEC, error_type=type(exc).__name__)

    # -- internals -----------------------------------------------------------

    def _attempt(self, state):
        g = state.get(RESERVED_KEY)
        if isinstance(g, dict):
            a = g.get("attempt")
            if isinstance(a, dict):
                return a
        return None

    def _set_attempt(self, state, attempt):
        g = state.get(RESERVED_KEY)
        if not isinstance(g, dict):
            g = {}
            state[RESERVED_KEY] = g
        if attempt is None:
            g.pop("attempt", None)
        else:
            g["attempt"] = attempt

    def _snapshot(self, state, repos):
        return {r: target(state, r) for r in repos}

    def _fallback(self, state, snapshot, now):
        delays = [retry_delay(failures(state, r) + 1) for r in snapshot]
        return now + max(delays)

    def _record_failure(self, state, repos, now):
        for r in repos:
            v = _entry(state, r)
            f = int(v.get("failures") or 0) + 1
            v["failures"] = f
            v["retry_due"] = now + retry_delay(f)

    def _start_gate(self, state, attempt, now):
        """Restart recovery deadline: the attempt-start fallback deadline and
        any persisted known-failure deadline among the snapshot targets, the
        durable one taking precedence (never earlier than either)."""
        gate = attempt.get("fallback_deadline")
        if not isinstance(gate, (int, float)):
            gate = self.now()
        for r in attempt.get("snapshot", {}):
            due = retry_due(state, r)
            if isinstance(due, (int, float)) and due > gate:
                gate = due
        return gate

    def _tick(self, state):
        now = self.now()
        attempt = self._attempt(state)
        if attempt is not None:
            self._advance(state, attempt, now)
            return
        dirty = dirty_repos(state)
        if not dirty:
            return
        eligible = [r for r in dirty
                    if retry_due(state, r) is None or now >= retry_due(state, r)]
        if not eligible:
            return

        snapshot = {r: target(state, r) for r in dirty}
        attempt = {
            "phase": "pre_create",
            "start_requested": False,
            "exec_id": None,
            "container_id": None,
            "fallback_deadline": self._fallback(state, snapshot, now),
            "snapshot": snapshot,
        }
        self._set_attempt(state, attempt)
        if not self.save(state):
            # Suppress the dependent create; the in-memory attempt is dropped
            # so nothing enables an action without durable state.
            self._set_attempt(state, None)
            log("error", PERSIST_FAILURE, repo=safe_repo(eligible[0]),
                not_persisted=True)
            return

        self._create(state, attempt, now)

    def _create(self, state, attempt, now):
        try:
            cid = self.client.container_id()
            eid = self.client.exec_create(cid)
        except Exception as exc:
            status = safe_status(getattr(exc, "status", None))
            log("error", EXEC_CREATE_ERROR, status=status,
                error_type=type(exc).__name__)
            # A definite create failure/timeout cannot have started the
            # command: count an observed failure for every snapshot target.
            self._set_attempt(state, None)
            self._record_failure(state, attempt["snapshot"], now)
            if not self.save(state):
                log("error", PERSIST_FAILURE, not_persisted=True)
                return
            log("info", RETRY_SCHEDULED,
                repos=[safe_repo(r) for r in sorted(attempt["snapshot"])],
                retry_delay=retry_delay(1))
            return
        attempt["exec_id"] = eid
        attempt["container_id"] = cid
        attempt["phase"] = "pre_start"
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)
            return
        self._maybe_start(state, attempt, now, immediate=True)

    def _maybe_start(self, state, attempt, now, immediate=False):
        gate = self._start_gate(state, attempt, now)
        if not immediate and now < gate:
            return
        attempt["start_requested"] = True
        attempt["phase"] = "pre_start"
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)
            return
        self._start(state, attempt, now, inspect_on_reject=False)

    def _start(self, state, attempt, now, inspect_on_reject=False):
        eid = attempt.get("exec_id")
        if not isinstance(eid, str) or not _ID_RE.match(eid):
            log("error", UNRESOLVED_EXEC, reason="invalid_saved_id")
            return
        try:
            self.client.exec_start(eid)
        except Exception as exc:
            status = safe_status(getattr(exc, "status", None))
            log("error", EXEC_START_UNCONFIRMED, status=status,
                error_type=type(exc).__name__)
            if status == 409:
                # Already-run responses are reconciled only by inspecting the
                # saved ID; if this cycle already inspected it, wait until next.
                if not inspect_on_reject:
                    self._inspect_terminal(state, attempt, now)
                return
            if isinstance(status, int) and 400 <= status < 500:
                if inspect_on_reject:
                    self._observed_start_failure(state, attempt, now)
                else:
                    self._observe_start_rejection(state, attempt, now)
                return
            attempt["phase"] = "started"
            self.save(state)
            return
        attempt["phase"] = "started"
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)

    def _observe_start_rejection(self, state, attempt, now):
        try:
            running, exit_code = self.client.exec_inspect(attempt["exec_id"])
        except Exception as exc:
            log("error", EXEC_INSPECT_ERROR, error_type=type(exc).__name__)
            return
        if running:
            return
        if exit_code is not None:
            if isinstance(exit_code, int) and exit_code == 0:
                self._terminal_success(state, attempt, now)
            else:
                self._terminal_failure(state, attempt, now, exit_code)
            return
        self._observed_start_failure(state, attempt, now)

    def _observed_start_failure(self, state, attempt, now):
        repos = sorted(attempt.get("snapshot", {}))
        self._set_attempt(state, None)
        self._record_failure(state, repos, now)
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)
            return
        log("info", RETRY_SCHEDULED,
            repos=[safe_repo(r) for r in repos],
            retry_delay=max(retry_delay(failures(state, r)) for r in repos))

    def _advance(self, state, attempt, now):
        """Attempt exists: recover creation or inspect at most once."""
        if not attempt.get("exec_id"):
            if now >= self._start_gate(state, attempt, now):
                self._create(state, attempt, now)
            return
        if not attempt.get("start_requested"):
            self._maybe_start(state, attempt, now)
            return
        self._inspect_terminal(state, attempt, now)

    def _inspect_terminal(self, state, attempt, now):
        eid = attempt.get("exec_id")
        if not isinstance(eid, str) or not _ID_RE.match(eid):
            log("error", UNRESOLVED_EXEC, reason="invalid_saved_id")
            return
        try:
            running, exit_code = self.client.exec_inspect(eid)
        except Exception as exc:
            status = safe_status(getattr(exc, "status", None))
            # Uninspectable ID / container replacement: unresolved, blocks all
            # repos; elapsed time never authorizes replacement or replay.
            if status == "unknown":
                log("error", EXEC_INSPECT_ERROR, error_type=type(exc).__name__)
            else:
                log("error", UNRESOLVED_EXEC, status=status)
            return
        if running:
            log("info", EXEC_RUNNING)
            return
        if exit_code is None:
            gate = self._start_gate(state, attempt, now)
            if now >= gate:
                self._start(state, attempt, now, inspect_on_reject=True)
            return
        if isinstance(exit_code, int) and exit_code == 0:
            self._terminal_success(state, attempt, now)
        else:
            self._terminal_failure(state, attempt, now, exit_code)

    def _terminal_success(self, state, attempt, now):
        snapshot = attempt.get("snapshot", {})
        # Persist the success ack for the snapshot BEFORE clearing the
        # attempt (design.md terminal exit 0).
        for r, gen in sorted(snapshot.items()):
            if target(state, r) == gen:
                _acknowledge(state, r, gen)
            else:
                # Record what succeeded, but retain retry state for the newer
                # generation that was not in this workspace snapshot.
                _entry(state, r)["completed"] = gen
        if not self.save(state):
            # Terminal-transition persistence failed: retain the ID and
            # inspect again next cycle; never create a replacement.
            log("error", PERSIST_FAILURE, not_persisted=True)
            return
        self._set_attempt(state, None)
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)
        log("info", EXEC_SUCCEEDED,
            repos=[safe_repo(r) for r in sorted(snapshot)])
        # A follow-on for newer dirty targets is started on a later cycle by
        # the top-level scheduler; never more than one exec per tick.

    def _terminal_failure(self, state, attempt, now, exit_code):
        snapshot = attempt.get("snapshot", {})
        # One workspace exit code cannot identify per-file failures: every
        # dirty target at failure observation is affected, including targets
        # that arrived after the snapshot.
        affected = dirty_repos(state) or list(snapshot)
        self._set_attempt(state, None)
        self._record_failure(state, affected, now)
        log("error", EXEC_FAILED, exit_code=exit_code,
            repos=[safe_repo(r) for r in sorted(affected)])
        if not self.save(state):
            log("error", PERSIST_FAILURE, not_persisted=True)


def build_from_env(environ=None, save=None, now=time.time):
    """Build a controller from config; absent DOCKER_HOST disables refresh."""
    if environ is None:
        from os import environ as config
    else:
        config = environ
    host = config.get("DOCKER_HOST")
    if not host:
        return None
    try:
        timeout = float(config.get("GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS", "10"))
        if not 0 < timeout < float("inf"):
            raise ValueError
        client = DockerClient(host, request_timeout=timeout)
    except (ValueError, TypeError):
        log("error", UNRESOLVED_EXEC, reason="invalid_config")
        return None
    return GraphifyRefresh(client, now=now, save=save)