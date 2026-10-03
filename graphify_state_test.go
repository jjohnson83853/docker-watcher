package watcher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// stateGraphifyClient records observable Docker interactions and can coordinate
// an ExecStart without timing assumptions.
type stateGraphifyClient struct {
	calls        []string
	inspection   ExecInspection
	inspectErr   error
	containerErr error
	createErr    error
	startErr     error
	startEntered chan struct{}
	startRelease chan struct{}
}

func (c *stateGraphifyClient) ContainerID(context.Context) (string, error) {
	c.calls = append(c.calls, "container")
	return "container-id", c.containerErr
}
func (c *stateGraphifyClient) ExecCreate(context.Context, string, []string, []string) (string, error) {
	c.calls = append(c.calls, "create")
	return "abcdef012345", c.createErr
}
func (c *stateGraphifyClient) ExecStart(ctx context.Context, _ string) error {
	c.calls = append(c.calls, "start")
	if c.startEntered != nil {
		close(c.startEntered)
		<-c.startRelease
	}
	return c.startErr
}
func (c *stateGraphifyClient) ExecInspect(context.Context, string) (ExecInspection, error) {
	c.calls = append(c.calls, "inspect")
	return c.inspection, c.inspectErr
}

type cancelDuringContainerLookupClient struct {
	cancel context.CancelFunc
	calls  []string
}

func (c *cancelDuringContainerLookupClient) ContainerID(context.Context) (string, error) {
	c.calls = append(c.calls, "container")
	c.cancel()
	return "container-id", nil
}
func (c *cancelDuringContainerLookupClient) ExecCreate(context.Context, string, []string, []string) (string, error) {
	c.calls = append(c.calls, "create")
	return "abcdef012345", nil
}
func (c *cancelDuringContainerLookupClient) ExecStart(context.Context, string) error {
	c.calls = append(c.calls, "start")
	return nil
}
func (c *cancelDuringContainerLookupClient) ExecInspect(context.Context, string) (ExecInspection, error) {
	c.calls = append(c.calls, "inspect")
	return ExecInspection{}, nil
}

func TestGraphifyDoesNotCreateExecAfterRunContextCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelDuringContainerLookupClient{cancel: cancel}
	graph := NewGraphify(client, nil, func() int64 { return 10 }, nil)
	graph.ctx = ctx
	graph.Tick(State{"repo": map[string]any{"target": int64(1)}})
	if strings.Join(client.calls, ",") != "container" {
		t.Fatalf("Graphify continued Docker work after cancellation: %v", client.calls)
	}
}

type cancelDuringExecCreateClient struct {
	cancel context.CancelFunc
	calls  []string
}

func (c *cancelDuringExecCreateClient) ContainerID(context.Context) (string, error) {
	c.calls = append(c.calls, "container")
	return "container-id", nil
}
func (c *cancelDuringExecCreateClient) ExecCreate(context.Context, string, []string, []string) (string, error) {
	c.calls = append(c.calls, "create")
	c.cancel()
	return "abcdef012345", nil
}
func (c *cancelDuringExecCreateClient) ExecStart(context.Context, string) error {
	c.calls = append(c.calls, "start")
	return nil
}
func (c *cancelDuringExecCreateClient) ExecInspect(context.Context, string) (ExecInspection, error) {
	c.calls = append(c.calls, "inspect")
	return ExecInspection{}, nil
}

func TestGraphifyPersistsExecIDReturnedDuringCancellationWithoutStartingIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelDuringExecCreateClient{cancel: cancel}
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph := NewGraphify(client, nil, func() int64 { return 10 }, nil)
	graph.ctx = ctx
	graph.Tick(state)
	attempt := graphAttempt(state)
	if nestedString(state, "graphify", "attempt", "exec_id") != "abcdef012345" ||
		nestedString(state, "graphify", "attempt", "phase") != "pre_start" || strings.Join(client.calls, ",") != "container,create" {
		t.Fatalf("successful exec creation was not durably recovered after cancellation: attempt=%#v calls=%v", attempt, client.calls)
	}
}

func TestGraphifyDoesNotStartAfterCancellationDuringStartPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &stateGraphifyClient{}
	saves := 0
	graph := NewGraphify(client, func(State) error {
		saves++
		if saves == 3 {
			cancel()
		}
		return nil
	}, func() int64 { return 10 }, nil)
	graph.ctx = ctx
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph.Tick(state)
	if strings.Join(client.calls, ",") != "container,create" || !nestedBool(state, "graphify", "attempt", "start_requested") {
		t.Fatalf("Graphify started after cancellation during durable start intent: calls=%v state=%#v", client.calls, state)
	}
}

func TestGraphifyStateSkipsCleanAndFutureRetryUntilDeadline(t *testing.T) {
	client := &stateGraphifyClient{}
	state := State{
		"clean":   map[string]any{"target": int64(2), "completed": int64(2)},
		"waiting": map[string]any{"target": int64(3), "completed": int64(1), "retry_due": int64(101)},
	}
	now := int64(100)
	graph := NewGraphify(client, nil, func() int64 { return now }, nil)
	graph.Tick(state)
	if len(client.calls) != 0 {
		t.Fatalf("future retry or clean state made Docker calls: %v", client.calls)
	}
	now = 101
	graph.Tick(state)
	waitForCapture(t, graph)
	if strings.Join(client.calls, ",") != "container,create,start" {
		t.Fatalf("deadline did not resume eligible state: %v", client.calls)
	}
	if nestedInt(state, "waiting", "completed") != 1 {
		t.Fatalf("creation prematurely acknowledged work: %#v", state)
	}
}

func TestGraphifyStateCompletionAcknowledgesOnlyAttemptSnapshot(t *testing.T) {
	client := &stateGraphifyClient{inspection: ExecInspection{ExitCode: intPtr(0)}}
	state := State{"repo-a": map[string]any{"target": int64(4)}}
	graph := NewGraphify(client, nil, func() int64 { return 100 }, nil)
	graph.Tick(state)
	waitForCapture(t, graph)
	state["repo-a"].(map[string]any)["target"] = int64(5)
	state["repo-b"] = map[string]any{"target": int64(1)}
	graph.Tick(state)
	if nestedInt(state, "repo-a", "completed") != 4 || nestedInt(state, "repo-b", "completed") != 0 || graphAttempt(state) != nil {
		t.Fatalf("completion did not acknowledge precisely the saved snapshot: %#v", state)
	}
	before := len(client.calls)
	graph.Tick(state)
	waitForCapture(t, graph)
	if strings.Join(client.calls[before:], ",") != "container,create,start" {
		t.Fatalf("later dirty generations were not handled in one subsequent operation: %v", client.calls[before:])
	}
	if nestedInt(state, "repo-a", "completed") != 4 || nestedInt(state, "repo-b", "completed") != 0 {
		t.Fatalf("second attempt acknowledged before terminal success: %#v", state)
	}
}
func TestSafeReposCapsSanitizedSortedRepositoryNames(t *testing.T) {
	names := make([]string, 105)
	for i := range names {
		names[i] = fmt.Sprintf("repo-%03d", i)
	}
	names[3] = "private/name"
	got := safeRepos(names)
	if len(got) != 100 {
		t.Fatalf("safeRepos returned %d entries; want historical maximum 100", len(got))
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("safeRepos output is not sorted: %v", got)
	}
	foundSanitizedName := false
	for _, name := range got {
		if name == "private/name" {
			t.Fatal("unsafe repository name was not sanitized")
		}
		if name == "unknown" {
			foundSanitizedName = true
		}
	}
	if !foundSanitizedName {
		t.Fatal("sanitized repository name was absent from the bounded output")
	}
}

func TestGraphifyStateFailedExecIDSaveSuppressesStart(t *testing.T) {
	client := &stateGraphifyClient{}
	saves := 0
	graph := NewGraphify(client, func(state State) error {
		saves++
		if saves == 2 {
			return errors.New("disk unavailable")
		}
		return nil
	}, func() int64 { return 20 }, nil)
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph.Tick(state)
	attempt := graphAttempt(state)
	if strings.Join(client.calls, ",") != "container,create" || attempt == nil || nestedString(state, "graphify", "attempt", "exec_id") != "" {
		t.Fatalf("unsaved exec ID proceeded to start or replaced the pre-create intent: calls=%v state=%#v", client.calls, state)
	}
}

func TestGraphifyStateFailedStartIntentSuppressesStart(t *testing.T) {
	client := &stateGraphifyClient{}
	saves := 0
	graph := NewGraphify(client, func(State) error {
		saves++
		if saves == 3 {
			return errors.New("read-only")
		}
		return nil
	}, func() int64 { return 20 }, nil)
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph.Tick(state)
	if countString(client.calls, "start") != 0 || nestedBool(state, "graphify", "attempt", "start_requested") {
		t.Fatalf("failed start-intent persistence allowed a start: calls=%v state=%#v", client.calls, state)
	}
}

func TestGraphifyStateCreateFailuresPersistRetryAndBoundedLogs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*stateGraphifyClient)
	}{
		{"container lookup", func(c *stateGraphifyClient) { c.containerErr = &DockerAPIError{StatusCode: 503} }},
		{"exec create", func(c *stateGraphifyClient) { c.createErr = &DockerAPIError{StatusCode: 400} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &stateGraphifyClient{}
			tc.configure(client)
			var logs strings.Builder
			state := State{"repo": map[string]any{"target": int64(1)}}
			graph := NewGraphify(client, nil, func() int64 { return 100 }, &logs)
			graph.Tick(state)
			if graphAttempt(state) != nil || nestedInt(state, "repo", "failures") != 1 || nestedInt(state, "repo", "retry_due") != 160 {
				t.Fatalf("failure did not persist retry transition: %#v", state)
			}
			if strings.Contains(logs.String(), "private token") || !strings.Contains(logs.String(), "exec_create_error") {
				t.Fatalf("failure log was not bounded/categorized: %s", logs.String())
			}
		})
	}
}

func TestGraphifyStateRetrySaveFailureSuppressesFurtherActions(t *testing.T) {
	client := &stateGraphifyClient{createErr: errors.New("private")}
	saves := 0
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph := NewGraphify(client, func(State) error {
		saves++
		if saves == 2 {
			return errors.New("disk")
		}
		return nil
	}, func() int64 { return 5 }, nil)
	graph.Tick(state)
	if strings.Join(client.calls, ",") != "container,create" || graphAttempt(state) == nil || nestedInt(state, "repo", "failures") != 0 {
		t.Fatalf("failed retry-state save did not restore pre-create state: calls=%v state=%#v", client.calls, state)
	}
}

func TestGraphifyStateRunningExecIsInspectedOnceAndNeverReattached(t *testing.T) {
	client := &stateGraphifyClient{inspection: ExecInspection{Running: true}}
	graph := NewGraphify(client, nil, func() int64 { return 1_000 }, nil)
	state := savedAttemptState("abcdef012345")
	graph.Tick(state)
	graph.Tick(state)
	if countString(client.calls, "inspect") != 2 || countString(client.calls, "start") != 0 || countString(client.calls, "create") != 0 {
		t.Fatalf("saved running exec was replayed or tick inspected incorrectly: %v", client.calls)
	}
}

func TestGraphifyStateTerminalInspectRetainsGuardWhileCaptureAlive(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	client := &stateGraphifyClient{inspection: ExecInspection{ExitCode: intPtr(0)}, startEntered: entered, startRelease: release}
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph := NewGraphify(client, nil, func() int64 { return 10 }, nil)
	graph.Tick(state)
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("attached start did not reach worker")
	}
	graph.Tick(state)
	if graphAttempt(state) == nil || nestedInt(state, "repo", "completed") != 0 || graph.worker == nil || !graph.worker.IsAlive() {
		close(release)
		t.Fatal("terminal inspect discarded attempt while capture was still active")
	}
	close(release)
	waitForCapture(t, graph)
	graph.Tick(state)
	if graphAttempt(state) != nil || nestedInt(state, "repo", "completed") != 1 {
		t.Fatalf("attempt did not complete after capture exited: %#v", state)
	}
}

func TestGraphifyStateNon409DefiniteStartFailureSchedulesRetry(t *testing.T) {
	client := &stateGraphifyClient{inspection: ExecInspection{}, startErr: &DockerAPIError{StatusCode: 400}}
	state := savedAttemptState("abcdef012345")
	graph := NewGraphify(client, nil, func() int64 { return 200 }, nil)
	graph.Tick(state)
	<-graph.worker.Done()
	graph.Tick(state)
	if graphAttempt(state) != nil || nestedInt(state, "repo", "failures") != 1 || nestedInt(state, "repo", "retry_due") != 260 {
		t.Fatalf("definite 4xx start failure did not transition to retry: %#v", state)
	}
}

func nestedBool(root State, keys ...string) bool {
	var value any = map[string]any(root)
	for _, key := range keys {
		parent, ok := value.(map[string]any)
		if !ok {
			return false
		}
		value = parent[key]
	}
	result, _ := value.(bool)
	return result
}

func waitForCapture(t *testing.T, graph *Graphify) {
	t.Helper()
	if graph.worker == nil {
		t.Fatal("expected capture worker")
	}
	select {
	case <-graph.worker.Done():
	case <-time.After(time.Second):
		t.Fatal("capture worker did not finish")
	}
}
