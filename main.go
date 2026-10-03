package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func Run(ctx context.Context, config Config) error {
	client := NewGitHubClient("", config.GitHubToken, nil, os.Stdout)
	graphify := newGraphifyTicker(ctx, config, os.Stdout)
	if config.GraphifyInvalidConfig {
		WriteLog(os.Stdout, "error", "graphify_refresh", map[string]any{"outcome": "unresolved_exec", "reason": "invalid_config"})
	}
	return runLoop(ctx, config, client, graphify, os.Stdout)
}

func newGraphifyTicker(ctx context.Context, config Config, logs io.Writer) GraphifyTicker {
	if !config.GraphifyEnabled {
		return nil
	}
	docker := NewDockerClient(config.DockerHost, config.GraphifyRequestTimeout)
	graph := NewGraphify(docker, func(state State) error { return SaveState(config.StateFile, state) }, nil, logs)
	graph.ctx = ctx
	return graph
}

func runLoop(ctx context.Context, config Config, client RepositoryClient, graphify GraphifyTicker, logs io.Writer) error {
	defer func() {
		if shutdown, ok := graphify.(interface{ Shutdown() }); ok {
			shutdown.Shutdown()
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		state, stateErr := LoadState(config.StateFile)
		if stateErr != nil {
			logTo(logs, "error", "poll_cycle_failed", map[string]any{"error_type": "StateError"})
			_ = touchHeartbeat(config.HeartbeatFile)
		} else {
			repos, listErr := client.ListOwnerRepos(ctx, config.GitHubUsername)
			if listErr != nil {
				logTo(logs, "error", "poll_cycle_failed", map[string]any{"error_type": "GitHubError"})
				_ = touchHeartbeat(config.HeartbeatFile)
			} else {
				processor := &Watcher{Config: config, Repositories: client, StatePath: config.StateFile, State: state, Logs: logs}
				_ = RunCycle(ctx, CycleRequest{Repositories: repos, HeartbeatPath: config.HeartbeatFile, Processor: processor})
			}
			if graphify != nil && ctx.Err() == nil {
				graphify.Tick(state)
			}
		}
		timer := time.NewTimer(time.Duration(config.PollIntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func ConfigFromEnvironment() (Config, error) {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	return LoadConfig(env)
}
func Start() error {
	config, err := ConfigFromEnvironment()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, config)
}
func RunWithConfig(ctx context.Context, config Config) error { return Run(ctx, config) }
func ConfigurationError(err error) string {
	if err == nil {
		return "configuration error"
	}
	var safeError configurationError
	if errors.As(err, &safeError) {
		return "configuration error: " + safeError.Error()
	}
	return fmt.Sprintf("configuration error: %T", err)
}
