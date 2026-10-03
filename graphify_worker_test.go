package watcher

import (
	"context"
	"testing"
	"time"
)

func TestCaptureWorkerCancellationUnblocksAndPublishesNoLateResult(t *testing.T) {
	started := make(chan struct{})
	worker := NewCaptureWorker(func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "workspace_path_missing", ctx.Err()
	})
	worker.Start()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("capture worker did not start")
	}
	worker.Cancel()
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("capture worker did not terminate after cancellation")
	}
	if got := worker.Result(); got != "unavailable" {
		t.Fatalf("late partial capture result must be discarded after cancellation: %q", got)
	}
}

func TestCaptureWorkerRetainsSingleFlightUntilOwnedCaptureExits(t *testing.T) {
	release := make(chan struct{})
	worker := NewCaptureWorker(func(context.Context) (string, error) {
		<-release
		return "unclassified", nil
	})
	worker.Start()
	select {
	case <-worker.Done():
		t.Fatal("blocked capture reported completion early")
	default:
	}
	worker.Cancel()
	select {
	case <-worker.Done():
		t.Fatal("capture released its slot before the owned operation exited")
	default:
	}
	close(release)
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("capture worker did not release single-flight slot on exit")
	}
	if got := worker.Result(); got != "unavailable" {
		t.Fatalf("canceled capture result = %q", got)
	}
}
