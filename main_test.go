package watcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type cancelingRepositoryClient struct {
	cancel context.CancelFunc
	repos  []Repository
	err    error
	calls  int
}

func (c *cancelingRepositoryClient) ListOwnerRepos(context.Context, string) ([]Repository, error) {
	c.calls++
	c.cancel()
	return c.repos, c.err
}

func (c *cancelingRepositoryClient) HasDockerfile(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (c *cancelingRepositoryClient) LatestCommitSHA(context.Context, string, string, string) (string, error) {
	return "", nil
}

type graphifyCallCounter struct{ calls int }

func (g *graphifyCallCounter) Tick(State) { g.calls++ }

type cancelingLogWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelingLogWriter) Write(data []byte) (int, error) {
	n, err := w.Buffer.Write(data)
	w.cancel()
	return n, err
}

func TestRunLoopRunsCycleAndSkipsGraphifyAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelingRepositoryClient{cancel: cancel}
	graphify := &graphifyCallCounter{}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	config := Config{GitHubUsername: "owner", StateFile: filepath.Join(t.TempDir(), "state.json"), HeartbeatFile: heartbeat, PollIntervalSeconds: 300}
	if err := runLoop(ctx, config, client, graphify, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || graphify.calls != 0 {
		t.Fatalf("service cycle calls: repositories=%d graphify=%d; want graphify skipped after cancellation", client.calls, graphify.calls)
	}
	if _, err := os.Stat(heartbeat); err != nil {
		t.Fatalf("cycle heartbeat missing after context cancellation: %v", err)
	}
}

func TestRunLoopStateReadFailureLogsAndTouchesHeartbeat(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &cancelingLogWriter{cancel: cancel}
	client := &cancelingRepositoryClient{cancel: func() {}}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	config := Config{GitHubUsername: "owner", StateFile: filepath.Join(parentFile, "state.json"), HeartbeatFile: heartbeat, PollIntervalSeconds: 300}
	if err := runLoop(ctx, config, client, nil, logs); err != nil {
		t.Fatal(err)
	}
	if client.calls != 0 || !strings.Contains(logs.String(), `"event":"poll_cycle_failed"`) || !strings.Contains(logs.String(), `"error_type":"StateError"`) {
		t.Fatalf("state read failure handling mismatch: calls=%d log=%q", client.calls, logs.String())
	}
	if _, err := os.Stat(heartbeat); err != nil {
		t.Fatalf("heartbeat missing after state read failure: %v", err)
	}
}

func TestRunLoopGitHubFailureSkipsGraphifyAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelingRepositoryClient{cancel: cancel, err: errors.New("private GitHub response")}
	graphify := &graphifyCallCounter{}
	logs := &bytes.Buffer{}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	config := Config{GitHubUsername: "owner", StateFile: filepath.Join(t.TempDir(), "state.json"), HeartbeatFile: heartbeat, PollIntervalSeconds: 300}
	if err := runLoop(ctx, config, client, graphify, logs); err != nil {
		t.Fatal(err)
	}
	if graphify.calls != 0 || !strings.Contains(logs.String(), `"error_type":"GitHubError"`) || strings.Contains(logs.String(), "private GitHub response") {
		t.Fatalf("GitHub failure handling mismatch: graphify=%d log=%q", graphify.calls, logs.String())
	}
	if _, err := os.Stat(heartbeat); err != nil {
		t.Fatalf("heartbeat missing after GitHub failure: %v", err)
	}
}

func TestPublicRunWrappersHandleCanceledContextAndConfigErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	config := Config{GitHubUsername: "owner", GitHubToken: "token", StateFile: filepath.Join(t.TempDir(), "state.json"), HeartbeatFile: filepath.Join(t.TempDir(), "heartbeat")}
	if err := Run(ctx, config); err != nil {
		t.Fatalf("Run with canceled context: %v", err)
	}
	if err := RunWithConfig(ctx, config); err != nil {
		t.Fatalf("RunWithConfig with canceled context: %v", err)
	}
	t.Setenv("GITHUB_USERNAME", "")
	t.Setenv("GITHUB_TOKEN", "")
	if err := Start(); err == nil {
		t.Fatal("Start accepted missing credentials")
	} else if !strings.HasPrefix(ConfigurationError(err), "configuration error:") || strings.Contains(ConfigurationError(err), "token") {
		t.Fatalf("configuration error rendering exposed details: %q", ConfigurationError(err))
	}
}
