# docker-watcher Go conversion — Design

Status: **confirmed**. Behavior-preserving conversion; the detailed approved baseline is in `.superpowers/sdd/docker-watcher-go-conversion/design.md` and `.omp/plans/docker-watcher-go-conversion.md`.

## Runtime architecture

The Go service uses the root `watcher` package with a `cmd/watcher` executable. Responsibilities remain separated across configuration/state/logging, GitHub discovery, build orchestration, Docker Engine/Graphify lifecycle, attached stream parsing, and the sequential polling loop. Use standard-library HTTP and narrow Docker Engine API calls. Successful GitHub JSON responses are limited to 8 MiB and repository pages to 100 entries, while full pagination remains uncapped. Retain `git` and pinned `docker-cli=29.8.2-r0` subprocess behavior; bound all child processes and never render tokenized clone command details.

After cloning, obtain the checked-out SHA, remove `.git`, and build. Persist the refresh target immediately after successful build and before any tag push. Persist final build SHA/pushed_at after both sequential pushes. Atomic state replacement preserves unknown keys and exact JSON integer precision, retains all existing Graphify state (including legacy bare-string SHA entries), uses one absolute cleaned state-file path for all I/O and rejects a symlink at the leaf. Before each save/load, sync lexical ancestry, every containing directory traversed by symlinks, and final resolved ancestry; after rename, sync the containing directory. Any sync failure fails closed. The state volume and ancestry must remain operator-controlled and stable; path-based checks do not prevent concurrent local mutation.

Graphify is serialized around the durable saved attempt. Save each enabling transition before its external action; inspect at most once in each poll cycle. A saved ambiguous ID blocks replacement indefinitely. A same-ID retry is permitted only after inspect proves not running with null exit status and the persisted fallback/known-failure deadline passes. Reconcile 409 via inspection of that ID. A capture worker owns attached start and stream parsing, cannot mutate state, and retains the in-memory single-flight guard until it exits. Recovered attempts are inspected, never reattached.

The attached transport supports TCP and Unix sockets, HTTP response framing (content-length and chunked/trailers), fragmented multiplex frames, strict caps and fixed diagnostic labels. The 64 KiB cap limits bytes scanned/retained; after reaching it, drain and discard through EOF or cancellation so the attached exec does not block. Bound resolution/connect/header setup and close sockets on cancellation. Do not add a total Graphify command timeout. The preserved Docker TCP endpoint on port 2375 is plaintext and unauthenticated; isolate it on a trusted network or protect it with a tunnel.

## Image and Compose

Build a static Go binary in a pinned Go builder stage; use a pinned Alpine runtime with CA certificates, Git, and pinned Docker CLI only. Preserve image/tag `registry.localdomain:5000/docker-watcher:latest`, exact `container_name: docker-watcher`, `.env`, `/data` named volume, `restart: unless-stopped`, heartbeat health threshold (poll interval + build timeout + 120 seconds) and 2200-second start period, CPU 2 / memory 1GiB, JSON log rotation, Watchtower exclusion, and `DOCKER_HOST=tcp://docker-lxc.localdomain:2375`. No socket mount, registry login, extra service, telemetry, CI, or live operation.

## Verification and cutover

All behavioral tests use injected/local fakes and scripted TCP/unix peers. No live GitHub, remote Git, daemon, registry, deployment, or Graphify calls. Full Go source-line coverage target is 90% or greater. The main orchestrator performs project-wide tests/build/lint after concurrent implementation work has landed.

Operators retain the old image reference, stop and verify the Python writer, inspect/back up the quiescent state, then exclusively start Go. Inspect each existing exec ID before Graphify work; an uninspectable ID blocks Graphify until independent proof and separately authorized state repair. Rollback stops Go and retains current state unless all later writes are reconciled. Never run both versions against state, tags, or Graphify.
