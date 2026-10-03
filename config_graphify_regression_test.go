package watcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type localRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip localRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestRunWithoutDockerHostCompletesLocalCycleWithNilGraphifyTicker(t *testing.T) {
	config, err := LoadConfig(map[string]string{
		"GITHUB_USERNAME": "owner",
		"GITHUB_TOKEN":    "local-test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.GraphifyEnabled {
		t.Fatal("Graphify should be disabled without DOCKER_HOST")
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	config.StateFile = statePath
	config.HeartbeatFile = filepath.Join(t.TempDir(), "heartbeat")
	config.PollIntervalSeconds = 30
	if err := SaveState(statePath, State{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelTimer := time.AfterFunc(20*time.Millisecond, cancel)
	defer cancelTimer.Stop()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = localRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	defer func() {
		http.DefaultTransport = oldTransport
		cancel()
	}()
	if err := Run(ctx, config); err != nil {
		t.Fatalf("Run returned an error after the local service cycle: %v", err)
	}
}

func TestGraphifyRequestTimeoutAcceptsFractionalSeconds(t *testing.T) {
	config, err := LoadConfig(map[string]string{
		"GITHUB_USERNAME": "owner",
		"GITHUB_TOKEN":    "local-test-token",
		"DOCKER_HOST":     "tcp://127.0.0.1:2375",
		"GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS": "1.25",
	})
	if err != nil {
		t.Fatalf("positive fractional Graphify timeout rejected: %v", err)
	}
	if got := config.GraphifyRequestTimeout; got != 1250*time.Millisecond {
		t.Fatalf("Graphify request timeout = %s; want %s", got, 1250*time.Millisecond)
	}
}

func TestInvalidGraphifyTimeoutDisablesOnlyGraphify(t *testing.T) {
	config, err := LoadConfig(map[string]string{
		"GITHUB_USERNAME": "owner",
		"GITHUB_TOKEN":    "local-test-token",
		"DOCKER_HOST":     "tcp://127.0.0.1:2375",
		"GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS": "not-a-duration",
	})
	if err != nil {
		t.Fatalf("invalid optional Graphify timeout became fatal: %v", err)
	}
	if config.GraphifyEnabled {
		t.Fatal("invalid optional Graphify timeout must disable Graphify")
	}
	if config.PollIntervalSeconds != 300 {
		t.Fatalf("repository polling configuration changed: interval=%d", config.PollIntervalSeconds)
	}
}

func TestMissingGitHubCredentialsErrorNamesKeysWithoutValues(t *testing.T) {
	const secret = "credential-must-not-appear"
	_, err := LoadConfig(map[string]string{"GITHUB_USERNAME": secret})
	if err == nil {
		t.Fatal("missing GITHUB_TOKEN accepted")
	}
	message := ConfigurationError(err)
	if !strings.Contains(message, "GITHUB_TOKEN") || !strings.Contains(message, "GITHUB_USERNAME") {
		t.Fatalf("credential error is not actionable: %q", message)
	}
	if strings.Contains(message, secret) {
		t.Fatalf("credential error disclosed a supplied secret: %q", message)
	}
}

func TestGraphifyLifecycleJSONPreservesEventAndOutcome(t *testing.T) {
	client := &stateGraphifyClient{createErr: &DockerAPIError{StatusCode: 400}}
	state := State{"owner/repo": map[string]any{"target": int64(1)}}
	var logs strings.Builder
	graph := NewGraphify(client, nil, func() int64 { return 100 }, &logs)
	graph.Tick(state)

	var event struct {
		Event   string `json:"event"`
		Outcome string `json:"outcome"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var candidate struct {
			Event   string `json:"event"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("Graphify lifecycle log is not JSON: %v; line=%q", err, line)
		}
		if candidate.Outcome == "exec_create_error" || candidate.Event == "exec_create_error" {
			event = candidate
			found = true
		}
	}
	if !found {
		t.Fatalf("exec-create failure lifecycle event not found: %s", logs.String())
	}
	if event.Event != "graphify_refresh" || event.Outcome != "exec_create_error" {
		t.Fatalf("Graphify lifecycle event = %+v; want graphify_refresh/exec_create_error", event)
	}
}
