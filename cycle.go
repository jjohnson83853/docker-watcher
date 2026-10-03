package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RepositoryProcessor interface {
	ProcessRepository(context.Context, Repository) error
}
type GraphifyTicker interface{ Tick(State) }

type CycleRequest struct {
	Repositories  []Repository
	HeartbeatPath string
	Processor     RepositoryProcessor
}

func RunCycle(ctx context.Context, request CycleRequest) error {
	var failures []error
	if request.Processor != nil {
		for _, repo := range request.Repositories {
			if err := request.Processor.ProcessRepository(ctx, repo); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if request.HeartbeatPath != "" {
		if err := touchHeartbeat(request.HeartbeatPath); err != nil {
			failures = append(failures, fmt.Errorf("touch heartbeat"))
		}
	}
	return errors.Join(failures...)
}

func touchHeartbeat(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

type Watcher struct {
	Config       Config
	Repositories RepositoryClient
	StatePath    string
	State        State
	Graphify     GraphifyTicker
	Logs         io.Writer
}

func (w *Watcher) ProcessRepository(ctx context.Context, repo Repository) error {
	err := w.processRepository(ctx, repo)
	if err == nil {
		return nil
	}
	branch := w.Config.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}
	fields := map[string]any{"repo": repo.FullName, "branch": branch, "error_type": errorType(err)}
	failure := RenderBuildFailure(err)
	fields["exit_code"] = failure.ExitCode
	if failure.OutputTail != "" {
		fields["output_tail"] = failure.OutputTail
	}
	logTo(w.Logs, "error", "build failed", fields)
	return err
}

func (w *Watcher) processRepository(ctx context.Context, repo Repository) error {
	stateKey := repositoryStateKey(w.State, repo)
	oldSHA, oldPushed := StateEntry(w.State, stateKey)
	if repo.PushedAt != "" && repo.PushedAt == oldPushed {
		return nil
	}
	branch := w.Config.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}
	owner := repo.Owner.Login
	name := repo.Name
	if owner == "" {
		parts := strings.SplitN(repo.FullName, "/", 2)
		if len(parts) == 2 {
			owner, name = parts[0], parts[1]
		}
	}
	hasDockerfile, err := w.Repositories.HasDockerfile(ctx, owner, name, branch)
	if err != nil {
		return err
	}
	if !hasDockerfile {
		return nil
	}
	sha, err := w.Repositories.LatestCommitSHA(ctx, owner, name, branch)
	if err != nil {
		return err
	}
	if sha == "" || sha == oldSHA {
		return nil
	}
	image := imageName(repo.FullName, w.Config.GitHubUsername)
	logTo(w.Logs, "info", "build_started", map[string]any{"repo": repo.FullName, "branch": branch, "sha": shortSHA(sha), "image": w.Config.RegistryURL + "/" + image})
	request := BuildRequest{CloneURL: repo.CloneURL, Branch: branch, Token: w.Config.GitHubToken, RegistryURL: w.Config.RegistryURL, Image: image, DockerBuildkit: w.Config.DockerBuildkit, Timeout: time.Duration(w.Config.BuildTimeoutSeconds) * time.Second}
	builtSHA, err := BuildAndPush(ctx, request, func(string) error {
		prior, hadPrior := w.State[stateKey]
		backup := cloneJSONValue(prior)
		entry := stateMap(w.State, stateKey)
		entry["target"] = stateInt(entry, "target") + 1
		if saveErr := SaveState(w.StatePath, w.State); saveErr != nil {
			if hadPrior {
				w.State[stateKey] = backup
			} else {
				delete(w.State, stateKey)
			}
			logTo(w.Logs, "error", "graphify_refresh", map[string]any{"repo": safeRepo(stateKey), "outcome": "persist_failure", "not_persisted": true})
			return saveErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	entry := stateMap(w.State, stateKey)
	entry["sha"], entry["pushed_at"] = builtSHA, repo.PushedAt
	if err := SaveState(w.StatePath, w.State); err != nil {
		return fmt.Errorf("save build state")
	}
	logTo(w.Logs, "info", "build_success", map[string]any{"repo": repo.FullName, "branch": branch, "sha": shortSHA(builtSHA), "image": w.Config.RegistryURL + "/" + image})
	return nil
}

func imageName(fullName, username string) string {
	prefix := username + "/"
	if len(fullName) >= len(prefix) && strings.EqualFold(fullName[:len(prefix)], prefix) {
		return fullName[len(prefix):]
	}
	return fullName
}

func stateMap(state State, name string) map[string]any {
	if entry, ok := state[name].(map[string]any); ok {
		return entry
	}
	if legacy, ok := state[name].(string); ok {
		entry := map[string]any{"sha": legacy}
		state[name] = entry
		return entry
	}
	entry := map[string]any{}
	state[name] = entry
	return entry
}
func stateInt(entry map[string]any, key string) int64 {
	value, _ := numberValue(entry[key])
	return value
}
func cloneJSONValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(current))
		for key, nested := range current {
			clone[key] = cloneJSONValue(nested)
		}
		return clone
	case []any:
		clone := make([]any, len(current))
		for index, nested := range current {
			clone[index] = cloneJSONValue(nested)
		}
		return clone
	case []string:
		return append([]string(nil), current...)
	default:
		return current
	}
}
func logTo(out io.Writer, level, event string, fields map[string]any) {
	if out != nil {
		WriteLog(out, level, event, fields)
	}
}
