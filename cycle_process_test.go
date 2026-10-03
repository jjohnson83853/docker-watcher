package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type processRepositoryClient struct {
	hasDockerfile   bool
	sha             string
	dockerfileCalls int
	shaCalls        int
}

func (c *processRepositoryClient) ListOwnerRepos(context.Context, string) ([]Repository, error) {
	return nil, nil
}
func (c *processRepositoryClient) HasDockerfile(context.Context, string, string, string) (bool, error) {
	c.dockerfileCalls++
	return c.hasDockerfile, nil
}
func (c *processRepositoryClient) LatestCommitSHA(context.Context, string, string, string) (string, error) {
	c.shaCalls++
	return c.sha, nil
}

func TestProcessRepositorySkipsUnchangedPushAndAbsentDockerfile(t *testing.T) {
	client := &processRepositoryClient{hasDockerfile: false, sha: "new-sha"}
	watcher := Watcher{Config: Config{Branch: "main"}, Repositories: client, State: State{"owner/repo": map[string]any{"sha": "old-sha", "pushed_at": "same"}}}
	if err := watcher.ProcessRepository(context.Background(), Repository{FullName: "owner/repo", PushedAt: "same"}); err != nil {
		t.Fatal(err)
	}
	if client.dockerfileCalls != 0 || client.shaCalls != 0 {
		t.Fatalf("unchanged push triggered GitHub calls: %#v", client)
	}
	watcher.State["owner/repo"] = map[string]any{"sha": "old-sha", "pushed_at": "prior"}
	if err := watcher.ProcessRepository(context.Background(), Repository{FullName: "owner/repo", PushedAt: "current"}); err != nil {
		t.Fatal(err)
	}
	if client.dockerfileCalls != 1 || client.shaCalls != 0 {
		t.Fatalf("absent Dockerfile lookup sequence = %#v", client)
	}
}

func TestProcessRepositoryPersistsFinalStateAfterBothTagsPushed(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events")
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf '0123456789abcdef0123456789abcdef01234567\n'; fi
`, `#!/bin/sh
printf '%s\n' "$*" >> "$EVENTS"
if [ "$1" = push ]; then test -f "$STATE_SAVED" || exit 9; fi
`)
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("EVENTS", events)
	t.Setenv("STATE_SAVED", statePath)
	client := &processRepositoryClient{hasDockerfile: true, sha: "0123456789abcdef0123456789abcdef01234567"}
	watcher := Watcher{Config: Config{GitHubUsername: "owner", GitHubToken: "token", RegistryURL: "registry", BuildTimeoutSeconds: 3}, Repositories: client, StatePath: statePath, State: State{}}
	// Each push checks that durable target state exists; final SHA/pushed_at persist afterward.
	if err := watcher.ProcessRepository(context.Background(), Repository{Name: "repo", FullName: "owner/repo", CloneURL: "https://github.com/owner/repo.git", DefaultBranch: "main", PushedAt: "now"}); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if nestedString(saved, "repo", "sha") != client.sha || nestedString(saved, "repo", "pushed_at") != "now" || nestedInt(saved, "repo", "target") != 1 {
		t.Fatalf("unexpected durable repository state: %#v", saved)
	}
	output, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(output), "push "); got != 2 {
		t.Fatalf("expected both tags pushed, output=%q", output)
	}
}

func TestProcessRepositoryLogsBuildFailureFields(t *testing.T) {
	client := &processRepositoryClient{hasDockerfile: true, sha: "new-sha"}
	installFakeExecutables(t, `#!/bin/sh
case "$1" in clone) exit 7 ;; esac
`, `#!/bin/sh
exit 0
`)
	var logs strings.Builder
	watcher := Watcher{Config: Config{GitHubUsername: "owner", GitHubToken: "token", RegistryURL: "registry", BuildTimeoutSeconds: 2}, Repositories: client, StatePath: filepath.Join(t.TempDir(), "state.json"), State: State{}, Logs: &logs}
	err := watcher.ProcessRepository(context.Background(), Repository{Name: "repo", FullName: "owner/repo", DefaultBranch: "main"})
	if err == nil {
		t.Fatal("expected subprocess build failure")
	}
	for _, field := range []string{`"event":"build failed"`, `"repo":"owner/repo"`, `"branch":"main"`, `"error_type"`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("failure log lacks %s: %s", field, logs.String())
		}
	}
}

func TestTargetSaveFailureLogsGraphifyPersistenceOutcome(t *testing.T) {
	events := filepath.Join(t.TempDir(), "docker-events")
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf '0123456789abcdef0123456789abcdef01234567\n'; fi
`, `#!/bin/sh
printf '%s\n' "$1" >> "$EVENTS"
`)
	t.Setenv("EVENTS", events)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	client := &processRepositoryClient{hasDockerfile: true, sha: "0123456789abcdef0123456789abcdef01234567"}
	var logs bytes.Buffer
	watcher := Watcher{
		Config:       Config{GitHubUsername: "owner", GitHubToken: "secret-token", RegistryURL: "registry", BuildTimeoutSeconds: 3},
		Repositories: client,
		StatePath:    statePath,
		State:        State{},
		Logs:         &logs,
	}
	err := watcher.ProcessRepository(context.Background(), Repository{
		Name: "bad/name", FullName: "owner/repo", CloneURL: "https://github.com/owner/repo.git",
		DefaultBranch: "main", PushedAt: "now",
	})
	if err == nil {
		t.Fatal("state persistence failure was ignored")
	}
	if len(watcher.State) != 0 {
		t.Fatalf("failed target save left an in-memory target: %#v", watcher.State)
	}
	for _, field := range []string{`"event":"graphify_refresh"`, `"outcome":"persist_failure"`, `"not_persisted":true`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("target-save failure log lacks %s: %s", field, logs.String())
		}
	}
	var safeEventFound bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid structured log %q: %v", line, err)
		}
		if record["event"] == "graphify_refresh" {
			safeEventFound = true
			if record["repo"] != "unknown" {
				t.Fatalf("unsafe repository identifier in Graphify event: %#v", record["repo"])
			}
		}
	}
	if !safeEventFound {
		t.Fatalf("Graphify persistence event missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), statePath) || strings.Contains(logs.String(), "secret-token") {
		t.Fatalf("target-save failure disclosed path or credential: %s", logs.String())
	}
	dockerEvents, readErr := os.ReadFile(events)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(dockerEvents)) != "build" {
		t.Fatalf("target-save failure allowed a tag push: %q", dockerEvents)
	}
}

func TestRunCycleContinuesAfterProcessFailureAndTouchesHeartbeat(t *testing.T) {
	processor := &scriptedRepoProcessor{fail: map[string]bool{"owner/bad": true}}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	err := RunCycle(context.Background(), CycleRequest{Repositories: []Repository{{FullName: "owner/bad"}, {FullName: "owner/good"}}, Processor: processor, HeartbeatPath: heartbeat})
	if err == nil {
		t.Fatal("expected repository failure")
	}
	if !equalStrings(processor.processed, []string{"owner/bad", "owner/good"}) {
		t.Fatalf("processing did not continue: %v", processor.processed)
	}
	if _, statErr := os.Stat(heartbeat); statErr != nil {
		t.Fatalf("heartbeat not touched: %v", statErr)
	}
}
