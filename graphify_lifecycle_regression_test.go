package watcher

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestGraphifyRetainsCompletedCaptureDiagnosticAfterTerminalInspect(t *testing.T) {
	client := &stateGraphifyClient{
		inspection: ExecInspection{},
		startErr:   &captureDiagnosticError{class: "workspace_path_missing"},
	}
	state := savedAttemptState("abcdef012345")
	var logs strings.Builder
	graph := NewGraphify(client, nil, func() int64 { return 200 }, &logs)
	graph.Tick(state)
	waitForCapture(t, graph)

	client.inspection = ExecInspection{ExitCode: intPtr(7)}
	graph.Tick(state)
	if !strings.Contains(logs.String(), `"diagnostic_class":"workspace_path_missing"`) {
		t.Fatalf("completed capture diagnostic was not retained after terminal inspect: %s", logs.String())
	}
}

func TestGraphifyRetryLogMatchesPersistedRetryDelay(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{"repo": map[string]any{"target": int64(2), "failures": int64(1)}}
	client := &stateGraphifyClient{createErr: errors.New("private Docker detail")}
	var logs strings.Builder
	graph := NewGraphify(client, func(state State) error { return SaveState(statePath, state) }, func() int64 { return 200 }, &logs)
	graph.Tick(state)

	persisted, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	retryDue := nestedInt(persisted, "repo", "retry_due")
	if retryDue == 0 {
		t.Fatalf("retry transition was not persisted: %#v", persisted)
	}
	var retryEvent struct {
		Event      string `json:"event"`
		Outcome    string `json:"outcome"`
		RetryDelay int64  `json:"retry_delay"`
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var candidate struct {
			Event      string `json:"event"`
			Outcome    string `json:"outcome"`
			RetryDelay int64  `json:"retry_delay"`
		}
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("invalid lifecycle JSON %q: %v", line, err)
		}
		if candidate.Outcome == "retry_scheduled" {
			retryEvent = candidate
			break
		}
	}
	if retryEvent.Event != "graphify_refresh" || retryEvent.RetryDelay != retryDue-200 {
		t.Fatalf("retry event=%+v, persisted retry_due=%d; want delay %d", retryEvent, retryDue, retryDue-200)
	}
}
