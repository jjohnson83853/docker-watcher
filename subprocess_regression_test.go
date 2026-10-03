package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSubprocessTimeoutKillsLingeringDescendantAfterParentExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	installFakeExecutables(t, `#!/bin/sh
case "$3" in
  clone) mkdir -p "$9/.git" ;;
esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
if [ "$1" = build ]; then
  (exec sleep 30) &
  echo $! > "$DESCENDANT_PID_FILE"
  exit 0
fi
`)
	t.Setenv("DESCENDANT_PID_FILE", pidFile)
	started := time.Now()
	_, err := BuildAndPush(context.Background(), BuildRequest{
		CloneURL: "https://github.com/owner/repo", Branch: "main", Token: "local-test-token",
		RegistryURL: "registry.local", Image: "repo", Timeout: 100 * time.Millisecond,
	}, nil)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("build with a lingering descendant unexpectedly succeeded")
	}
	if elapsed > time.Second {
		t.Fatalf("subprocess command outlived timeout while waiting for descendant: %s", elapsed)
	}

	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake Docker did not record its child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		t.Fatalf("invalid child PID %q: %v", contents, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !processIsRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subprocess descendant remained alive after timeout")
}

func processIsRunning(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] != "Z"
}
