package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubRepositoryPaginationOwnershipAndAuthentication(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer sentinel-token" || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("GitHub compatibility headers missing: %v", r.Header)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			page := make([]Repository, 100)
			for i := range page {
				page[i] = Repository{FullName: fmt.Sprintf("Alice/repo-%03d", i), Owner: RepositoryOwner{Login: "Alice"}, DefaultBranch: "main", PushedAt: "old"}
			}
			page[0].Owner.Login = "other"
			_ = json.NewEncoder(w).Encode(page)
		case "2":
			_ = json.NewEncoder(w).Encode([]Repository{{FullName: "ALICE/last", Owner: RepositoryOwner{Login: "ALICE"}, DefaultBranch: "trunk"}})
		default:
			t.Errorf("unexpected page request: %s", r.URL)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(server.URL, "sentinel-token", server.Client(), nil)
	got, err := client.ListOwnerRepos(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 || got[len(got)-1].FullName != "ALICE/last" {
		t.Fatalf("wrong owned repositories: count=%d last=%+v", len(got), got[len(got)-1])
	}
	if len(requests) != 2 || !strings.Contains(requests[0], "per_page=100") || !strings.Contains(requests[0], "page=1") || !strings.Contains(requests[1], "page=2") {
		t.Fatalf("pagination contract broken: %v", requests)
	}
}

func TestGitHubEmptyRepositorySetIsVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	var logs strings.Builder
	client := NewGitHubClient(server.URL, "token", server.Client(), &logs)
	repos, err := client.ListOwnerRepos(context.Background(), "owner")
	if err != nil || len(repos) != 0 {
		t.Fatalf("empty repository list = %v, %v", repos, err)
	}
	if !strings.Contains(logs.String(), "repos_found") || !strings.Contains(logs.String(), "0") {
		t.Fatalf("zero repositories were not observable: %s", logs.String())
	}
}

func TestGitHubRepositoryListErrorIsVisibleWithoutLeakingResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private GitHub response /customer/repo", http.StatusForbidden)
	}))
	defer server.Close()
	var logs strings.Builder
	client := NewGitHubClient(server.URL, "sentinel-token", server.Client(), &logs)
	repos, err := client.ListOwnerRepos(context.Background(), "owner")
	if err != nil || len(repos) != 0 {
		t.Fatalf("failed list should preserve existing empty-result behavior: repos=%v err=%v", repos, err)
	}
	if !strings.Contains(logs.String(), "github_api_error") || !strings.Contains(logs.String(), "403") ||
		strings.Contains(logs.String(), "private GitHub response") || strings.Contains(logs.String(), "/customer/repo") ||
		strings.Contains(logs.String(), "sentinel-token") {
		t.Fatalf("GitHub failure visibility/redaction mismatch: %s", logs.String())
	}
}
func TestGitHubExpectedDockerfile404AndUnexpectedStatuses(t *testing.T) {
	statuses := map[string]int{"missing": http.StatusNotFound, "denied": http.StatusForbidden, "broken": http.StatusInternalServerError}
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.RequestURI())
		name := strings.TrimPrefix(r.URL.Path, "/repos/owner/")
		name = strings.TrimSuffix(name, "/contents/Dockerfile")
		w.WriteHeader(statuses[name])
	}))
	defer server.Close()
	var logs strings.Builder
	client := NewGitHubClient(server.URL, "token", server.Client(), &logs)
	for repo, want := range map[string]bool{"missing": false, "denied": false, "broken": false} {
		got, err := client.HasDockerfile(context.Background(), "owner", repo, "release/雪")
		if err != nil || got != want {
			t.Fatalf("HasDockerfile(%q) = %v, %v", repo, got, err)
		}
	}
	if strings.Contains(logs.String(), "404") || !strings.Contains(logs.String(), "403") || !strings.Contains(logs.String(), "500") {
		t.Fatalf("expected 404 to be quiet and abnormal statuses visible: %s", logs.String())
	}
	if len(requested) != 3 {
		t.Fatalf("expected one contents request per repo, got %v", requested)
	}
	for _, uri := range requested {
		if !strings.Contains(uri, "ref=release%2F") {
			t.Fatalf("branch was not safely encoded: %v", requested)
		}
	}
}

func TestGitHubLatestCommitErrorsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/good/commits/main":
			_, _ = w.Write([]byte(`{"sha":"abc123"}`))
		case "/repos/owner/malformed/commits/main":
			_, _ = w.Write([]byte("not-json"))
		default:
			http.Error(w, "private response", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	client := NewGitHubClient(server.URL, "token", server.Client(), nil)
	if got, err := client.LatestCommitSHA(context.Background(), "owner", "good", "main"); err != nil || got != "abc123" {
		t.Fatalf("valid commit response = %q, %v", got, err)
	}
	for _, repo := range []string{"malformed", "denied"} {
		if _, err := client.LatestCommitSHA(context.Background(), "owner", repo, "main"); err == nil {
			t.Errorf("expected %s response to fail", repo)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListOwnerRepos(ctx, "owner"); err == nil {
		t.Fatal("canceled GitHub request succeeded")
	}
}

func TestProcessRepositorySkipsUnchangedAndBuildsChangedRepository(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf 'already-built\n'; fi
`, `#!/bin/sh
exit 0
`)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{"owner/unchanged": map[string]any{"sha": "same", "pushed_at": "stamp"}}
	client := &scriptedRepositoryClient{repos: []Repository{
		{FullName: "owner/unchanged", Owner: RepositoryOwner{Login: "owner"}, Name: "unchanged", CloneURL: "https://github.com/owner/unchanged.git", DefaultBranch: "main", PushedAt: "stamp"},
		{FullName: "owner/changed", Owner: RepositoryOwner{Login: "owner"}, Name: "changed", CloneURL: "https://github.com/owner/changed.git", DefaultBranch: "trunk", PushedAt: "new"},
	}, latestSHA: "already-built"}
	watcher := &Watcher{
		Config:       Config{GitHubUsername: "owner", GitHubToken: "local-test-token", RegistryURL: "registry.local", BuildTimeoutSeconds: 3},
		Repositories: client, StatePath: statePath, State: state,
	}
	if err := watcher.ProcessRepository(context.Background(), client.repos[0]); err != nil {
		t.Fatal(err)
	}
	if err := watcher.ProcessRepository(context.Background(), client.repos[1]); err != nil {
		t.Fatal(err)
	}
	if client.dockerfileCalls != 1 || client.shaCalls != 1 || len(client.branches) != 2 || client.branches[0] != "trunk" || client.branches[1] != "trunk" {
		t.Fatalf("unexpected actual repository processing: Dockerfile=%d SHA=%d branches=%v", client.dockerfileCalls, client.shaCalls, client.branches)
	}
	if nestedString(watcher.State, "changed", "sha") != "already-built" {
		t.Fatalf("changed repository SHA was not persisted: %#v", watcher.State)
	}
}

func TestProcessRepositoryHonorsForcedBranchAndPropagatesLookupErrors(t *testing.T) {
	wantErr := errors.New("dockerfile lookup unavailable")
	client := &scriptedRepositoryClient{
		repos:         []Repository{{FullName: "owner/app", Owner: RepositoryOwner{Login: "owner"}, Name: "app", DefaultBranch: "trunk", PushedAt: "new"}},
		dockerfileErr: wantErr,
	}
	watcher := &Watcher{Config: Config{Branch: "release"}, Repositories: client, State: State{"owner/app": map[string]any{"sha": "old", "pushed_at": "old"}}}
	if err := watcher.ProcessRepository(context.Background(), client.repos[0]); !errors.Is(err, wantErr) {
		t.Fatalf("ProcessRepository error = %v; want %v", err, wantErr)
	}
	if client.dockerfileCalls != 1 || client.shaCalls != 0 || len(client.branches) != 1 || client.branches[0] != "release" {
		t.Fatalf("forced branch or lookup error handling changed: Dockerfile=%d SHA=%d branches=%v", client.dockerfileCalls, client.shaCalls, client.branches)
	}
}

type scriptedRepositoryClient struct {
	repos           []Repository
	dockerfileCalls int
	shaCalls        int
	branches        []string
	latestSHA       string
	dockerfileErr   error
	shaErr          error
}

func (c *scriptedRepositoryClient) ListOwnerRepos(context.Context, string) ([]Repository, error) {
	return c.repos, nil
}
func (c *scriptedRepositoryClient) HasDockerfile(_ context.Context, _, _, branch string) (bool, error) {
	c.dockerfileCalls++
	c.branches = append(c.branches, branch)
	return true, c.dockerfileErr
}
func (c *scriptedRepositoryClient) LatestCommitSHA(_ context.Context, _, _, branch string) (string, error) {
	c.shaCalls++
	c.branches = append(c.branches, branch)
	if c.shaErr != nil {
		return "", c.shaErr
	}
	if c.latestSHA != "" {
		return c.latestSHA, nil
	}
	return "different", nil
}

func TestBuildPersistsTargetAfterBuildBeforeSequentialPushes(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
printf '%s\n' "git $*" >> "$EVENTS"
case "$3" in clone) mkdir -p "$9/.git"; printf 'safe context' > "$9/Dockerfile" ;; esac
if [ "$1" = rev-parse ]; then printf '%s\n' '0123456789abcdef0123456789abcdef01234567'; fi
`, `#!/bin/sh
printf '%s\n' "docker $*" >> "$EVENTS"
case "$1" in
  build) test ! -e "$6/.git"; test "$(cat "$6/Dockerfile")" = 'safe context'; touch "$BUILD_DONE" ;;
  push) test -f "$TARGET_SAVED" ;;
esac
`)
	events := filepath.Join(t.TempDir(), "events")
	targetSaved := filepath.Join(t.TempDir(), "target-saved")
	buildDone := filepath.Join(t.TempDir(), "build-done")
	t.Setenv("EVENTS", events)
	t.Setenv("TARGET_SAVED", targetSaved)
	t.Setenv("BUILD_DONE", buildDone)
	var actions []string
	request := BuildRequest{CloneURL: "https://github.com/owner/repo.git", Branch: "main", Token: "sentinel-pat", RegistryURL: "registry.localdomain:5000", Image: "repo", Timeout: 3 * time.Second}
	sha, err := BuildAndPush(context.Background(), request, func(got string) error {
		if got != "0123456789abcdef0123456789abcdef01234567" {
			return fmt.Errorf("unexpected checked-out sha %q", got)
		}
		if _, err := os.Stat(buildDone); err != nil {
			return fmt.Errorf("target saved before successful build: %w", err)
		}
		actions = append(actions, "target-save")
		return SaveState(targetSaved, State{"owner/repo": map[string]any{"target": got}})
	})
	if err != nil || sha != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("BuildAndPush() = %q, %v", sha, err)
	}
	savedState, err := LoadState(targetSaved)
	if err != nil || nestedString(savedState, "owner/repo", "target") != sha {
		t.Fatalf("actual persisted target unavailable before/after push: state=%#v err=%v", savedState, err)
	}
	observed, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(observed)), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "git -c credential.helper=store --file=") || !strings.Contains(lines[0], " clone --depth 1 --branch main") || !strings.HasPrefix(lines[1], "git rev-parse HEAD") || !strings.HasPrefix(lines[2], "docker build") || !strings.HasPrefix(lines[3], "docker push") || !strings.HasPrefix(lines[4], "docker push") {
		t.Fatalf("unexpected subprocess order: %q", lines)
	}
	if len(actions) != 1 {
		t.Fatalf("target callback missing: %v", actions)
	}
	if !strings.Contains(lines[2], ":latest") || !strings.Contains(lines[2], ":0123456") || !strings.Contains(lines[3], ":latest") || !strings.Contains(lines[4], ":0123456") {
		t.Fatalf("expected latest and seven-character tags: %q", lines)
	}
}

func TestBuildTargetSaveFailurePreventsBothPushes(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
printf '%s\n' "$1" >> "$EVENTS"
`)
	events := filepath.Join(t.TempDir(), "docker-events")
	t.Setenv("EVENTS", events)
	_, err := BuildAndPush(context.Background(), BuildRequest{CloneURL: "https://github.com/o/r", Branch: "main", Token: "pat", RegistryURL: "reg", Image: "r", Timeout: time.Second}, func(string) error {
		return errors.New("state write failed")
	})
	if err == nil {
		t.Fatal("failed durable target save was ignored")
	}
	observed, readErr := os.ReadFile(events)
	if readErr != nil {
		t.Fatalf("expected build command to occur before target failure: %v", readErr)
	}
	if strings.TrimSpace(string(observed)) != "build" {
		t.Fatalf("push occurred despite failed target persistence: %q", observed)
	}
}

func TestBuildFailureOutputTailKeepsExactLast4000Characters(t *testing.T) {
	output := strings.Repeat("x", MaxBuildOutputChars+4) + "TAIL" + strings.Repeat("y", MaxBuildOutputChars-4)
	failure := RenderBuildFailure(&buildError{code: 7, output: output})
	want := "TAIL" + strings.Repeat("y", MaxBuildOutputChars-4)
	if len(failure.OutputTail) != MaxBuildOutputChars || failure.OutputTail != want {
		t.Fatalf("output tail length/content mismatch: len=%d suffix=%q", len(failure.OutputTail), failure.OutputTail[len(failure.OutputTail)-8:])
	}
}
func TestBuildFailureBoundsDiagnosticAndNeverLeaksCredential(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git"; printf 'safe context' > "$9/Dockerfile" ;; esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
if [ "$1" = build ]; then printf '%05000d sentinel-pat' 0 >&2; exit 7; fi
`)
	_, err := BuildAndPush(context.Background(), BuildRequest{CloneURL: "https://github.com/o/r", Branch: "main", Token: "sentinel-pat", RegistryURL: "reg", Image: "r", Timeout: time.Second}, nil)
	if err == nil {
		t.Fatal("failed fake docker build succeeded")
	}
	failure := RenderBuildFailure(err)
	if len(failure.OutputTail) > MaxBuildOutputChars || failure.Event != "build failed" {
		t.Fatalf("failure output/event contract violated: event=%q tail length=%d", failure.Event, len(failure.OutputTail))
	}
	encoded, marshalErr := json.Marshal(failure)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "sentinel-pat") {
		t.Fatalf("credential disclosed in rendered failure: %s", encoded)
	}
}

func TestCloneFailureDoesNotRenderTokenizedCommandOrSubprocessOutput(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
printf 'clone failure sentinel-pat' >&2
exit 9
`, `#!/bin/sh
exit 99
`)
	_, err := BuildAndPush(context.Background(), BuildRequest{CloneURL: "https://github.com/o/r", Branch: "main", Token: "sentinel-pat", RegistryURL: "reg", Image: "r", Timeout: time.Second}, nil)
	if err == nil {
		t.Fatal("fake clone failure was ignored")
	}
	if strings.Contains(err.Error(), "sentinel-pat") || strings.Contains(err.Error(), "clone failure") {
		t.Fatalf("clone command or output disclosed in error: %v", err)
	}
	if got := RenderBuildFailure(err).ExitCode; got != 9 {
		t.Fatalf("rendered Git clone exit code = %#v; want 9", got)
	}
}

func TestGitRevisionFailureRendersNumericExitCode(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then exit 11; fi
`, `#!/bin/sh
exit 99
`)
	_, err := BuildAndPush(context.Background(), BuildRequest{
		CloneURL: "https://github.com/o/r", Branch: "main", Token: "pat",
		RegistryURL: "reg", Image: "r", Timeout: time.Second,
	}, nil)
	if err == nil {
		t.Fatal("fake revision failure was ignored")
	}
	if got := RenderBuildFailure(err).ExitCode; got != 11 {
		t.Fatalf("rendered Git revision exit code = %#v; want 11", got)
	}
}

func TestGitCloneTimeoutRendersTimeoutClassification(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
if [ "$3" = clone ]; then sleep 30; fi
`, `#!/bin/sh
exit 99
`)
	_, err := BuildAndPush(context.Background(), BuildRequest{
		CloneURL: "https://github.com/o/r", Branch: "main", Token: "pat",
		RegistryURL: "reg", Image: "r", Timeout: 100 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("timed out fake clone was reported as successful")
	}
	if got := RenderBuildFailure(err).ExitCode; got != "timeout" {
		t.Fatalf("rendered Git clone timeout = %#v; want timeout", got)
	}
}

func TestLatestPushFailurePreventsShortSHACommit(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
if [ "$1" = push ]; then printf '%s\n' "$*" >> "$EVENTS"; exit 8; fi
`)
	events := filepath.Join(t.TempDir(), "push-events")
	t.Setenv("EVENTS", events)
	_, err := BuildAndPush(context.Background(), BuildRequest{CloneURL: "https://github.com/o/r", Branch: "main", Token: "pat", RegistryURL: "reg", Image: "r", Timeout: time.Second}, func(string) error { return nil })
	if err == nil {
		t.Fatal("failed latest push was reported as success")
	}
	observed, readErr := os.ReadFile(events)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(observed)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "push reg/r:latest") {
		t.Fatalf("latest push was not the first push action: %q", observed)
	}
	var pushes []string
	for _, line := range lines {
		if strings.HasPrefix(line, "push ") {
			pushes = append(pushes, line)
		}
	}
	if len(pushes) != 1 {
		t.Fatalf("short-SHA tag push continued after latest push failed: %q", observed)
	}
}
func TestBuildSubprocessTimeoutStopsFurtherActions(t *testing.T) {
	installFakeExecutables(t, `#!/bin/sh
case "$3" in clone) mkdir -p "$9/.git" ;; esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
if [ "$1" = build ]; then sleep 2; fi
`)
	started := time.Now()
	_, err := BuildAndPush(context.Background(), BuildRequest{CloneURL: "https://github.com/o/r", Branch: "main", Token: "pat", RegistryURL: "reg", Image: "r", Timeout: 30 * time.Millisecond}, nil)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("subprocess timeout was not enforced promptly: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestPollCycleContinuesAfterRepositoryFailureAndTouchesHeartbeat(t *testing.T) {
	processor := &scriptedRepoProcessor{fail: map[string]bool{"owner/bad": true}}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	err := RunCycle(context.Background(), CycleRequest{Repositories: []Repository{{FullName: "owner/bad"}, {FullName: "owner/good"}}, HeartbeatPath: heartbeat, Processor: processor})
	if err == nil {
		t.Fatal("cycle should report repository failure after processing all repositories")
	}
	if !equalStrings(processor.processed, []string{"owner/bad", "owner/good"}) {
		t.Fatalf("later repository was skipped: %v", processor.processed)
	}
	info, err := os.Stat(heartbeat)
	if err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("heartbeat was not touched after failed repo: info=%v err=%v", info, err)
	}
}

type scriptedRepoProcessor struct {
	processed []string
	fail      map[string]bool
}

func (p *scriptedRepoProcessor) ProcessRepository(_ context.Context, repo Repository) error {
	p.processed = append(p.processed, repo.FullName)
	if p.fail[repo.FullName] {
		return errors.New("private repository failure")
	}
	return nil
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func installFakeExecutables(t *testing.T, gitScript, dockerScript string) {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{"git": gitScript, "docker": dockerScript} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
