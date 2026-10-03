package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGraphifyPersistsIntentBeforeCreateAndOnlyOneInspectPerTick(t *testing.T) {
	client := &fakeGraphifyClient{}
	var snapshots []State
	graph := NewGraphify(client, func(state State) error {
		snapshots = append(snapshots, cloneState(state))
		return nil
	}, func() int64 { return 100 }, nil)
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph.Tick(state)
	calls := client.callSnapshot()
	if len(calls) == 0 || calls[0] != "container" {
		t.Fatalf("refresh did not start after durable intent: calls=%v", calls)
	}
	if len(snapshots) < 2 || nestedString(snapshots[0], "graphify", "attempt", "phase") != "pre_create" {
		t.Fatalf("attempt intent was not saved before external create: %#v", snapshots)
	}
	client.inspection = ExecInspection{Running: true}
	before := len(client.callSnapshot())
	graph.Tick(state)
	calls = client.callSnapshot()
	if countString(calls[before:], "inspect") != 1 {
		t.Fatalf("cycle must inspect persisted ID at most once: %v", calls[before:])
	}
	if countString(calls[before:], "create") != 0 {
		t.Fatalf("running attempt was replaced: %v", calls[before:])
	}
}

func TestGraphifyFailedPersistenceSuppressesExternalSideEffect(t *testing.T) {
	client := &fakeGraphifyClient{}
	graph := NewGraphify(client, func(State) error { return errors.New("disk path secret") }, func() int64 { return 100 }, nil)
	state := State{"repo": map[string]any{"target": int64(1)}}
	graph.Tick(state)
	if calls := client.callSnapshot(); len(calls) != 0 {
		t.Fatalf("Docker call happened without durable intent: %v", calls)
	}
}

func TestGraphifyUninspectableSavedIDBlocksReplacementIndefinitely(t *testing.T) {
	client := &fakeGraphifyClient{inspectErr: errors.New("private endpoint response")}
	state := savedAttemptState("abcdef012345")
	graph := NewGraphify(client, func(State) error { return nil }, func() int64 { return 1_000_000 }, nil)
	for range 4 {
		graph.Tick(state)
	}
	calls := client.callSnapshot()
	if countString(calls, "create") != 0 || countString(calls, "start") != 0 {
		t.Fatalf("ambiguous saved ID was replayed/replaced: %v", calls)
	}
	if nestedString(state, "graphify", "attempt", "exec_id") != "abcdef012345" {
		t.Fatalf("uninspectable attempt was discarded: %#v", state)
	}
}

func TestGraphifyRecoveredExecIsInspectedNeverReattached(t *testing.T) {
	client := &fakeGraphifyClient{inspection: ExecInspection{Running: false, ExitCode: intPtr(0)}}
	state := savedAttemptState("abcdef012345")
	graph := NewGraphify(client, func(State) error { return nil }, func() int64 { return 500 }, nil)
	graph.Tick(state)
	calls := client.callSnapshot()
	if countString(calls, "inspect") != 1 || countString(calls, "start") != 0 || countString(calls, "create") != 0 {
		t.Fatalf("recovered ID must be inspected only: %v", calls)
	}
	if graphAttempt(state) != nil || nestedInt(state, "repo", "completed") != 1 {
		t.Fatalf("successful recovered exec did not acknowledge snapshot: %#v", state)
	}
}

func TestGraphifyFailurePersistsCappedRetryStateWithoutRawDiagnostics(t *testing.T) {
	client := &fakeGraphifyClient{inspection: ExecInspection{ExitCode: intPtr(7)}}
	state := savedAttemptState("abcdef012345")
	var logs strings.Builder
	graph := NewGraphify(client, func(State) error { return nil }, func() int64 { return 1_000 }, &logs)
	graph.Tick(state)
	if nestedInt(state, "repo", "failures") != 1 || nestedInt(state, "repo", "retry_due") != 1_060 {
		t.Fatalf("failure retry state incorrect: %#v", state)
	}
	if strings.Contains(logs.String(), "private/path") || strings.Contains(logs.String(), "graphify raw output") {
		t.Fatalf("raw diagnostic disclosed: %s", logs.String())
	}
	if RetryDelay(0) != 60 || RetryDelay(1) != 60 || RetryDelay(2) != 120 || RetryDelay(6) != 1_800 || RetryDelay(20) != 1_800 {
		t.Fatalf("retry backoff changed: 0=%d 1=%d 2=%d 6=%d 20=%d", RetryDelay(0), RetryDelay(1), RetryDelay(2), RetryDelay(6), RetryDelay(20))
	}
}

func TestGraphifySameIDRetryRequiresExpiredPersistedGateAnd409Reconciliation(t *testing.T) {
	client := &fakeGraphifyClient{inspection: ExecInspection{Running: false}}
	state := savedAttemptState("abcdef012345")
	graph := NewGraphify(client, func(State) error { return nil }, func() int64 { return 99 }, nil)
	graph.Tick(state)
	calls := client.callSnapshot()
	if countString(calls, "start") != 0 {
		t.Fatalf("exec restarted before persisted retry gate: %v", calls)
	}
	graph.now = func() int64 { return 100 }
	client.startStatus = 409
	graph.Tick(state)
	select {
	case <-graph.worker.Done():
	case <-time.After(time.Second):
		t.Fatal("same-ID start worker did not finish")
	}
	calls = client.callSnapshot()
	if countString(calls, "start") != 1 || countString(calls, "inspect") != 2 || countString(calls, "create") != 0 {
		t.Fatalf("expired retry must start only the saved ID after inspect: %v", calls)
	}
	client.inspection = ExecInspection{Running: true}
	graph.Tick(state)
	calls = client.callSnapshot()
	if countString(calls, "inspect") != 3 || countString(calls, "start") != 1 || countString(calls, "create") != 0 {
		t.Fatalf("409 was not reconciled by a later inspect of the same ID: %v", calls)
	}
	if nestedString(state, "graphify", "attempt", "exec_id") != "abcdef012345" {
		t.Fatalf("same-ID retry changed persisted ID: %#v", state)
	}
}

func TestGraphifyTerminalSaveFailureRetainsAttemptAndDefersCompletion(t *testing.T) {
	client := &fakeGraphifyClient{inspection: ExecInspection{ExitCode: intPtr(0)}}
	failTerminalSave := true
	graph := NewGraphify(client, func(state State) error {
		if graphAttempt(state) == nil && nestedInt(state, "repo", "completed") == 1 && failTerminalSave {
			failTerminalSave = false
			return errors.New("persistence failed")
		}
		return nil
	}, func() int64 { return 100 }, nil)
	state := savedAttemptState("abcdef012345")
	graph.Tick(state)
	if graphAttempt(state) == nil {
		t.Fatal("failed terminal persistence released durable attempt")
	}
	graph.Tick(state)
	calls := client.callSnapshot()
	if countString(calls, "create") != 0 || nestedInt(state, "repo", "completed") != 1 {
		t.Fatalf("terminal retry created a replacement or lost completion: %#v calls=%v", state, calls)
	}
}

type fakeGraphifyClient struct {
	mu          sync.Mutex
	calls       []string
	inspection  ExecInspection
	inspectErr  error
	startStatus int
}

func (c *fakeGraphifyClient) recordCall(call string) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
}

func (c *fakeGraphifyClient) callSnapshot() []string {
	c.mu.Lock()
	calls := append([]string(nil), c.calls...)
	c.mu.Unlock()
	return calls
}

func (c *fakeGraphifyClient) ContainerID(context.Context) (string, error) {
	c.recordCall("container")
	return "container-id", nil
}
func (c *fakeGraphifyClient) ExecCreate(context.Context, string, []string, []string) (string, error) {
	c.recordCall("create")
	return "abcdef012345", nil
}
func (c *fakeGraphifyClient) ExecStart(context.Context, string) error {
	c.recordCall("start")
	if c.startStatus == 409 {
		return &DockerAPIError{StatusCode: 409}
	}
	return nil
}
func (c *fakeGraphifyClient) ExecInspect(context.Context, string) (ExecInspection, error) {
	c.recordCall("inspect")
	return c.inspection, c.inspectErr
}

func savedAttemptState(id string) State {
	return State{
		"repo": map[string]any{"target": int64(1)},
		"graphify": map[string]any{"attempt": map[string]any{
			"phase": "pre_start", "start_requested": true, "exec_id": id,
			"container_id": "container-id", "fallback_deadline": int64(100),
			"snapshot": map[string]any{"repo": int64(1)},
		}},
	}
}

func nestedMap(root State, keys ...string) map[string]any {
	var value any = map[string]any(root)
	for _, key := range keys {
		parent, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = parent[key]
	}
	result, _ := value.(map[string]any)
	return result
}
func nestedString(root State, keys ...string) string {
	var value any = map[string]any(root)
	for _, key := range keys {
		parent, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = parent[key]
	}
	result, _ := value.(string)
	return result
}
func nestedInt(root State, keys ...string) int64 {
	var value any = map[string]any(root)
	for _, key := range keys {
		parent, ok := value.(map[string]any)
		if !ok {
			return 0
		}
		value = parent[key]
	}
	number, _ := numberValue(value)
	return number
}
func setAttemptValue(state State, key string, value any) {
	attempt := graphAttempt(state)
	attempt[key] = value
}
func intPtr(value int) *int { return &value }
func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
func cloneState(state State) State {
	encoded, _ := json.Marshal(state)
	var clone State
	_ = json.Unmarshal(encoded, &clone)
	return clone
}
