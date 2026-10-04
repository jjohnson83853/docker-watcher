package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatIsFreshUsesStrictStalenessBoundary(t *testing.T) {
	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		pollInterval int
		buildTimeout int
		age          time.Duration
		want         bool
	}{
		{name: "fresh before default threshold", pollInterval: 300, buildTimeout: 1800, age: 2219 * time.Second, want: true},
		{name: "stale at default threshold", pollInterval: 300, buildTimeout: 1800, age: 2220 * time.Second, want: false},
		{name: "stale after default threshold", pollInterval: 300, buildTimeout: 1800, age: 2221 * time.Second, want: false},
		{name: "configured values set threshold", pollInterval: 17, buildTimeout: 23, age: 159 * time.Second, want: true},
		{name: "configured values are stale at threshold", pollInterval: 17, buildTimeout: 23, age: 160 * time.Second, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mtime := now.Add(-tc.age)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if got := HeartbeatIsFresh(path, tc.pollInterval, tc.buildTimeout, now); got != tc.want {
				t.Fatalf("HeartbeatIsFresh at age %s with intervals %d/%d = %t, want %t", tc.age, tc.pollInterval, tc.buildTimeout, got, tc.want)
			}
		})
	}
}

func TestHeartbeatIsFreshRejectsMissingOrUnreadableHeartbeat(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing")
	brokenLink := filepath.Join(directory, "unreadable")
	if err := os.Symlink(filepath.Join(directory, "absent-target"), brokenLink); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	for _, path := range []string{missing, brokenLink} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if HeartbeatIsFresh(path, 300, 1800, now) {
				t.Fatalf("HeartbeatIsFresh(%q) = true, want false", path)
			}
		})
	}
}
