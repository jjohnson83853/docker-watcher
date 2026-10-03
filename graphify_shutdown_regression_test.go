package watcher

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

type shutdownGraphifyClient struct {
	started chan struct{}
	stopped chan struct{}
}

func (c *shutdownGraphifyClient) ContainerID(context.Context) (string, error) {
	return "", errors.New("unexpected container lookup")
}
func (c *shutdownGraphifyClient) ExecCreate(context.Context, string, []string, []string) (string, error) {
	return "", errors.New("unexpected exec creation")
}
func (c *shutdownGraphifyClient) ExecStart(ctx context.Context, _ string) error {
	close(c.started)
	<-ctx.Done()
	close(c.stopped)
	return ctx.Err()
}
func (c *shutdownGraphifyClient) ExecInspect(context.Context, string) (ExecInspection, error) {
	return ExecInspection{}, nil
}

func TestRunLoopCancellationWaitsForCaptureExitAndRetainsDurableAttempt(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, savedAttemptState("abcdef012345")); err != nil {
		t.Fatal(err)
	}
	client := &shutdownGraphifyClient{started: make(chan struct{}), stopped: make(chan struct{})}
	graph := NewGraphify(client, func(state State) error { return SaveState(statePath, state) }, func() int64 { return 200 }, io.Discard)
	config := Config{StateFile: statePath, PollIntervalSeconds: 30}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runLoop(ctx, config, &scriptedRepositoryClient{}, graph, io.Discard) }()

	select {
	case <-client.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("runLoop did not start the persisted Graphify capture")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runLoop returned error after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runLoop did not wait for canceled capture worker to exit")
	}
	select {
	case <-client.stopped:
	default:
		t.Fatal("runLoop returned before the capture worker exited")
	}
	persisted, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if nestedString(persisted, "graphify", "attempt", "exec_id") != "abcdef012345" {
		t.Fatalf("shutdown discarded the durable Graphify attempt: %#v", persisted)
	}
}
