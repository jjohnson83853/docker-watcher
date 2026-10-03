# Go development

The service uses Go 1.24 and the standard library. Install Go 1.24 locally; Git and Docker CLI are needed only to exercise the subprocess boundary with local fakes or run the service in its intended runtime. Never point tests at live GitHub, remote Git, a Docker daemon, registry, or production Graphify.

## Build and run

```sh
go build ./...
go run ./cmd/watcher
```

Set required `GITHUB_USERNAME` and `GITHUB_TOKEN` through the environment for a local service run. Optional settings and defaults are documented in `requirements.md`. Use a temporary `STATE_FILE` and `HEARTBEAT_FILE` for local manual runs. The runtime image is not built by application contributors; image build and deployment remain operator-owned.

## Tests and checks

```sh
go test ./...
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go vet ./...
```

The approved target is at least 90% Go production source-line coverage. The suite uses local test servers, fake executables, temporary files, and scripted TCP/Unix peers. Project-wide validation is coordinated after concurrent implementation work has landed.

## Runtime contract

The Go executable replaces the Python service; there is no Python shim. State remains at `/data/state.json` by default and uses one absolute cleaned state-file path for all I/O; a symlink at the state-file leaf is rejected. State replacement is atomic with file sync, sync of lexical ancestry, every containing directory traversed by symlinks, and final resolved ancestry before each save, plus containing-directory sync after rename. Existing state is re-confirmed by syncing all ancestry before use; any failure prevents repository and Graphify work in that cycle. Keep the state volume and its ancestry operator-controlled and stable; path-based checks do not defend against concurrent local filesystem mutation. Compose retains `/data`, the configured heartbeat healthcheck, and the existing Docker TCP proxy endpoint. Port 2375 is plaintext and unauthenticated; keep it on a trusted isolated network or behind a protected tunnel. The Go and Python versions must never run concurrently against shared state, image tags, or Graphify work. Operators own exclusive cutover and rollback.
