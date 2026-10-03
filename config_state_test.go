package watcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigPreservesEnvironmentContract(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, Config)
	}{
		{
			name:    "missing required username",
			env:     map[string]string{"GITHUB_TOKEN": "secret"},
			wantErr: true,
		},
		{
			name:    "missing required token",
			env:     map[string]string{"GITHUB_USERNAME": "owner"},
			wantErr: true,
		},
		{
			name: "defaults and optional Graphify",
			env:  map[string]string{"GITHUB_USERNAME": "owner", "GITHUB_TOKEN": "token"},
			check: func(t *testing.T, got Config) {
				t.Helper()
				if got.RegistryURL != "registry.localdomain:5000" || got.StateFile != "/data/state.json" || got.PollIntervalSeconds != 300 || got.BuildTimeoutSeconds != 1800 || got.HeartbeatFile != "/data/heartbeat" || got.GraphifyRequestTimeout != 10*time.Second {
					t.Fatalf("unexpected defaults: %+v", got)
				}
				if got.Branch != "" || got.DockerHost != "" || got.DockerBuildkit != "0" || got.GraphifyEnabled {
					t.Fatalf("unexpected optional defaults: %+v", got)
				}
			},
		},
		{
			name: "explicit values and empty branch retained",
			env: map[string]string{
				"GITHUB_USERNAME": "owner", "GITHUB_TOKEN": "token", "BRANCH": "",
				"REGISTRY_URL": "registry.internal:5000", "STATE_FILE": "/tmp/state.json",
				"POLL_INTERVAL_SECONDS": "17", "BUILD_TIMEOUT_SECONDS": "23",
				"HEARTBEAT_FILE": "/tmp/beat", "DOCKER_HOST": "tcp://docker:2375",
				"DOCKER_BUILDKIT": "0", "GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS": "9",
			},
			check: func(t *testing.T, got Config) {
				t.Helper()
				if got.Branch != "" || got.RegistryURL != "registry.internal:5000" || got.StateFile != "/tmp/state.json" || got.PollIntervalSeconds != 17 || got.BuildTimeoutSeconds != 23 || got.HeartbeatFile != "/tmp/beat" || got.DockerHost != "tcp://docker:2375" || got.DockerBuildkit != "0" || got.GraphifyRequestTimeout != 9*time.Second {
					t.Fatalf("explicit environment values lost: %+v", got)
				}
			},
		},
		{
			name:    "invalid poll interval is rejected",
			env:     map[string]string{"GITHUB_USERNAME": "owner", "GITHUB_TOKEN": "token", "POLL_INTERVAL_SECONDS": "invalid"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadConfig(tt.env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidPollAndBuildDurations(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"POLL_INTERVAL_SECONDS", "-1"},
		{"BUILD_TIMEOUT_SECONDS", "-1"},
		{"POLL_INTERVAL_SECONDS", "9223372037"},
		{"BUILD_TIMEOUT_SECONDS", "9223372037"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			_, err := LoadConfig(map[string]string{
				"GITHUB_USERNAME": "owner", "GITHUB_TOKEN": "token",
				tc.key: tc.value,
			})
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("LoadConfig error = %v, want an error naming %s", err, tc.key)
			}
		})
	}
}
func TestLoadConfigAcceptsZeroPollAndBuildSeconds(t *testing.T) {
	config, err := LoadConfig(map[string]string{
		"GITHUB_USERNAME": "owner", "GITHUB_TOKEN": "token",
		"POLL_INTERVAL_SECONDS": "0", "BUILD_TIMEOUT_SECONDS": "0",
	})
	if err != nil {
		t.Fatalf("zero intervals were rejected: %v", err)
	}
	if config.PollIntervalSeconds != 0 || config.BuildTimeoutSeconds != 0 {
		t.Fatalf("zero seconds changed during parsing: %+v", config)
	}
}

func TestStateLoadSupportsMissingLegacyAndCurrentShapes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "nested", "state.json")
	state, err := LoadState(statePath)
	if err != nil || len(state) != 0 {
		t.Fatalf("missing state should load empty: state=%v err=%v", state, err)
	}
	contents := `{"legacy":"deadbeef","current":{"sha":"cafebabe","pushed_at":"2026-01-02T03:04:05Z","target":3,"completed":2,"failures":1,"retry_due":123},"graphify":{"attempt":{"exec_id":"abcdef123456"}},"future":{"kept":true}}`
	if err := os.WriteFile(statePath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	sha, pushedAt := StateEntry(state, "legacy")
	if sha != "deadbeef" || pushedAt != "" {
		t.Fatalf("legacy bare SHA changed: sha=%q pushed_at=%q", sha, pushedAt)
	}
	sha, pushedAt = StateEntry(state, "current")
	if sha != "cafebabe" || pushedAt != "2026-01-02T03:04:05Z" {
		t.Fatalf("current entry changed: sha=%q pushed_at=%q", sha, pushedAt)
	}
	if !strings.Contains(string(mustJSON(t, state)), `"exec_id":"abcdef123456"`) || !strings.Contains(string(mustJSON(t, state)), `"kept":true`) {
		t.Fatalf("Graphify or unknown state fields were lost: %s", mustJSON(t, state))
	}
}

func TestSaveStateAtomicallyReplacesAndKeepsPriorFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	prior := []byte(`{"repo":"old"}`)
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, State{"repo": map[string]any{"sha": "new"}, "unknown": true}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	sha, _ := StateEntry(loaded, "repo")
	if sha != "new" || loaded["unknown"] != true {
		t.Fatalf("saved state mismatch: %v", loaded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("temporary state artifact remains: %v", entries)
	}
	badPath := filepath.Join(dir, "not-a-directory", "state.json")
	if err := os.WriteFile(filepath.Dir(badPath), []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(badPath, State{"repo": "partial"}); err == nil {
		t.Fatal("save through a non-directory parent unexpectedly succeeded")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !json.Valid(actual) {
		t.Fatalf("failed save damaged prior state: bytes=%q err=%v", actual, err)
	}
}

func TestWriteLogEmitsJSONEventAndDoesNotRenderUnsafeError(t *testing.T) {
	var out strings.Builder
	WriteLog(&out, "error", "build failed", map[string]any{"repo": "team/app", "error_type": "BuildError", "secret": "must-not-be-logged"})
	var record map[string]any
	if err := json.Unmarshal([]byte(out.String()), &record); err != nil {
		t.Fatalf("log is not JSON: %q: %v", out.String(), err)
	}
	if record["level"] != "error" || record["event"] != "build failed" || record["repo"] != "team/app" {
		t.Fatalf("unexpected structured log contract: %v", record)
	}
	if strings.Contains(out.String(), "must-not-be-logged") {
		t.Fatalf("unsafe arbitrary field rendered: %s", out.String())
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStateRoundTripPreservesNestedUnknownValues(t *testing.T) {
	want := State{"repo": map[string]any{"sha": "x", "extension": []any{"unicode-雪", json.Number("4")}}, "graphify": map[string]any{"attempt": map[string]any{"phase": "pre_start"}}}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("semantic state round-trip mismatch: got %#v want %#v", got, want)
	}
}

func TestLoadAndSaveStatePreservesLargeUnknownIntegers(t *testing.T) {
	const exactInteger = "9007199254740993"
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"extension":{"nested":{"counter":9007199254740993}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	extension := state["extension"].(map[string]any)
	nested := extension["nested"].(map[string]any)
	if got, ok := nested["counter"].(json.Number); !ok || got.String() != exactInteger {
		t.Fatalf("decoded extension integer = %#v; want exact JSON number %s", nested["counter"], exactInteger)
	}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"counter":`+exactInteger) {
		t.Fatalf("saved extension integer lost precision: %s", data)
	}
}
