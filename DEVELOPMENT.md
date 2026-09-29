# Development tests

The watcher runtime uses only the Python standard library. Install its isolated
pytest tooling with:

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-dev.txt
.venv/bin/python -m pytest
```

The default pytest configuration measures both `app.py` and
`graphify_refresh.py`, failing below 90% line coverage. Tests use mocked GitHub
responses and Docker Engine clients; they do not require network or Docker.
