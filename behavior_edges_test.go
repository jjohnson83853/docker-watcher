package watcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildErrorPreservesCauseAndClassifiesCancellation(t *testing.T) {
	cause := errors.New("subprocess stopped")
	failure := &buildError{code: "timeout", cause: cause}
	if failure.Error() != "build failed" || !errors.Is(failure, cause) {
		t.Fatalf("build error lost its fixed message or cause: %v", failure)
	}
	if got := buildExitCode(context.Canceled); got != "timeout" {
		t.Fatalf("canceled build exit code = %#v, want timeout", got)
	}
	if got := buildExitCode(errors.New("launch failed")); got != "error" {
		t.Fatalf("unclassified build exit code = %#v, want error", got)
	}
}

func TestRunCycleReportsHeartbeatFailureAfterProcessing(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	processor := &recordingProcessor{}
	err := RunCycle(context.Background(), CycleRequest{
		Repositories:  []Repository{{Name: "repo"}},
		HeartbeatPath: filepath.Join(blocker, "heartbeat"),
		Processor:     processor,
	})
	if err == nil || err.Error() != "touch heartbeat" {
		t.Fatalf("RunCycle heartbeat failure = %v, want safe touch-heartbeat error", err)
	}
	if len(processor.processed) != 1 {
		t.Fatalf("heartbeat failure prevented repository processing: %v", processor.processed)
	}
}

func TestAttachedStreamRejectsUnsupportedSetupEndpoints(t *testing.T) {
	for _, tc := range []struct {
		host    string
		timeout time.Duration
	}{
		{host: "unsupported://daemon", timeout: time.Second},
		{host: "tcp://not-a-host-port", timeout: time.Second},
		{host: "tcp://127.0.0.1:1", timeout: 0},
	} {
		if body, err := NewAttachedStream(tc.host, tc.timeout).Start(context.Background(), "/start"); err == nil {
			_ = body.Close()
			t.Errorf("invalid stream endpoint accepted: host=%q timeout=%s", tc.host, tc.timeout)
		}
	}
}

func TestGraphifySafeDiagnosticsAndRepositoryNames(t *testing.T) {
	var logs bytes.Buffer
	graph := NewGraphify(nil, nil, nil, &logs)
	graph.log("error", "graphify_refresh", map[string]any{
		"repo":             safeRepo("../private/repo"),
		"diagnostic_class": safeDiagnostic("/private/path"),
		"error_type":       errorType(nil),
	})
	line := logs.String()
	if strings.Contains(line, "private") || strings.Contains(line, "../") || !strings.Contains(line, `"repo":"unknown"`) || !strings.Contains(line, `"diagnostic_class":"unclassified"`) || !strings.Contains(line, `"error_type":"unknown"`) {
		t.Fatalf("unsafe Graphify diagnostics escaped or safe fields were lost: %s", line)
	}
}

func TestGraphifyGenerationAcceptsPersistedNumericTypes(t *testing.T) {
	for _, value := range []any{int(7), int64(7), float64(7)} {
		if !sameGeneration(value, int64(7)) {
			t.Errorf("persisted generation type %T was not accepted", value)
		}
	}
	if sameGeneration("7", int64(7)) {
		t.Fatal("string generation was accepted as a numeric generation")
	}
}

func TestRunHandlesCanceledContextWithGraphifyEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	config := Config{
		GitHubUsername:         "owner",
		GitHubToken:            "token",
		StateFile:              filepath.Join(t.TempDir(), "state.json"),
		HeartbeatFile:          filepath.Join(t.TempDir(), "heartbeat"),
		DockerHost:             "unsupported://daemon",
		GraphifyRequestTimeout: time.Second,
		GraphifyEnabled:        true,
	}
	if err := Run(ctx, config); err != nil {
		t.Fatalf("Run with canceled context and Graphify enabled: %v", err)
	}
}

func TestGraphifyRecoveredExecWithoutFallbackDeadlineDoesNotRestart(t *testing.T) {
	client := &stateGraphifyClient{}
	graph := NewGraphify(client, nil, func() int64 { return 500 }, nil)
	state := State{
		"repo": map[string]any{"target": int64(1), "completed": int64(0)},
		"graphify": map[string]any{"attempt": map[string]any{
			"exec_id":  "abcdef012345",
			"snapshot": map[string]any{"repo": int64(1)},
		}},
	}
	graph.Tick(state)
	if len(client.calls) != 1 || client.calls[0] != "inspect" {
		t.Fatalf("recovered exec was restarted without a durable deadline: calls=%v", client.calls)
	}
}

func TestGraphifyContainsDependencyPanicInSafeEvent(t *testing.T) {
	var logs bytes.Buffer
	graph := NewGraphify(nil, nil, func() int64 { return 100 }, &logs)
	graph.Tick(State{"repo": map[string]any{"target": int64(1)}})
	if !strings.Contains(logs.String(), `"outcome":"unresolved_exec"`) || !strings.Contains(logs.String(), `"event":"graphify_refresh"`) {
		t.Fatalf("dependency panic was not contained by the poll tick: %s", logs.String())
	}
}

type recordingProcessor struct{ processed []Repository }

func (p *recordingProcessor) ProcessRepository(_ context.Context, repo Repository) error {
	p.processed = append(p.processed, repo)
	return nil
}
