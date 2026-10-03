package watcher

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRepositoryUpdatePreservesUnknownAndGraphifyState(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in
  clone) mkdir -p "$9/.git" ;;
esac
if [ "$1" = rev-parse ]; then printf '%s\n' '0123456789abcdef0123456789abcdef01234567'; fi
`, `#!/bin/sh
case "$1" in
  build) test ! -e "$6/.git" ;;
  push) test -f "$STATE_FILE" ;;
esac
`)
	statePath := filepath.Join(t.TempDir(), "state.json")
	initial := State{
		"repo": map[string]any{
			"target":    float64(3),
			"completed": 2,
			"future":    map[string]any{"labels": []any{"one", "two"}},
		},
		"graphify": map[string]any{"attempt": map[string]any{"exec_id": "abcdef123456"}},
	}
	if err := SaveState(statePath, initial); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATE_FILE", statePath)
	client := &processRepositoryClient{hasDockerfile: true, sha: "new-sha"}
	processor := Watcher{
		Config:       Config{GitHubUsername: "owner", GitHubToken: "token", RegistryURL: "registry", BuildTimeoutSeconds: 3},
		Repositories: client,
		StatePath:    statePath,
		State:        initial,
	}
	repo := Repository{Name: "repo", FullName: "owner/repo", CloneURL: "https://github.com/owner/repo.git", DefaultBranch: "main", PushedAt: "current"}
	if err := processor.ProcessRepository(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got["repo"].(map[string]any)
	if !ok || entry["target"] != json.Number("4") || entry["completed"] != json.Number("2") || entry["sha"] != "0123456789abcdef0123456789abcdef01234567" || entry["pushed_at"] != "current" {
		t.Fatalf("repository state was not updated without losing prior fields: %#v", got["repo"])
	}
	if !reflect.DeepEqual(entry["future"], map[string]any{"labels": []any{"one", "two"}}) || !reflect.DeepEqual(got["graphify"], map[string]any{"attempt": map[string]any{"exec_id": "abcdef123456"}}) {
		t.Fatalf("unknown or Graphify state was lost: %#v", got)
	}
}

func TestStateCompatibilityHelpersCloneAndConvertLegacyValues(t *testing.T) {
	state := State{"repo": "old-sha"}
	entry := stateMap(state, "repo")
	if entry["sha"] != "old-sha" || state["repo"] == "old-sha" {
		t.Fatalf("legacy SHA was not promoted for an update: %#v", state)
	}
	for _, value := range []any{int(4), int64(4), float64(4), json.Number("4")} {
		if got := stateInt(map[string]any{"target": value}, "target"); got != 4 {
			t.Errorf("stateInt(%T) = %d, want 4", value, got)
		}
	}
	if got := stateInt(map[string]any{"target": "invalid"}, "target"); got != 0 {
		t.Fatalf("invalid target = %d, want 0", got)
	}

	original := map[string]any{"nested": []any{map[string]any{"name": "stable"}}, "strings": []string{"a", "b"}}
	cloned := cloneJSONValue(original).(map[string]any)
	cloned["nested"].([]any)[0].(map[string]any)["name"] = "changed"
	cloned["strings"].([]string)[0] = "changed"
	if original["nested"].([]any)[0].(map[string]any)["name"] != "stable" || original["strings"].([]string)[0] != "a" {
		t.Fatalf("cloneJSONValue mutated original data: %#v", original)
	}
}

func TestNumberValuePreservesJSONIntegersAndRejectsOverflow(t *testing.T) {
	for _, test := range []struct {
		value any
		want  int64
		ok    bool
	}{
		{value: json.Number("9007199254740993"), want: 9007199254740993, ok: true},
		{value: json.Number("4.5"), want: 4, ok: true},
		{value: json.Number("9223372036854775808"), ok: false},
		{value: json.Number("invalid"), ok: false},
	} {
		got, ok := numberValue(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("numberValue(%#v) = %d, %t; want %d, %t", test.value, got, ok, test.want, test.ok)
		}
	}
}
