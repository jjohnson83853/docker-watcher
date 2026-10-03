package watcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	GitHubUsername         string
	GitHubToken            string
	Branch                 string
	RegistryURL            string
	StateFile              string
	PollIntervalSeconds    int
	BuildTimeoutSeconds    int
	HeartbeatFile          string
	DockerHost             string
	DockerBuildkit         string
	GraphifyRequestTimeout time.Duration
	GraphifyEnabled        bool
	GraphifyInvalidConfig  bool
}
type configurationError string

func (e configurationError) Error() string { return string(e) }

func LoadConfig(env map[string]string) (Config, error) {
	get := func(key, fallback string) string {
		if value, ok := env[key]; ok {
			return value
		}
		return fallback
	}
	username, token := get("GITHUB_USERNAME", ""), get("GITHUB_TOKEN", "")
	if username == "" || token == "" {
		missing := make([]string, 0, 2)
		if username == "" {
			missing = append(missing, "GITHUB_USERNAME")
		}
		if token == "" {
			missing = append(missing, "GITHUB_TOKEN")
		}
		return Config{}, configurationError(fmt.Sprintf("GITHUB_USERNAME and GITHUB_TOKEN are required (missing or empty: %s)", strings.Join(missing, ", ")))
	}
	parse := func(key string, fallback int) (int, error) {
		value := get(key, strconv.Itoa(fallback))
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || int64(n) > math.MaxInt64/int64(time.Second) {
			return 0, configurationError("invalid " + key + ": must be non-negative seconds within the supported duration range")
		}
		return n, nil
	}
	poll, err := parse("POLL_INTERVAL_SECONDS", 300)
	if err != nil {
		return Config{}, err
	}
	buildTimeout, err := parse("BUILD_TIMEOUT_SECONDS", 1800)
	if err != nil {
		return Config{}, err
	}
	host := get("DOCKER_HOST", "")
	graphTimeout := 10 * time.Second
	graphEnabled, invalidGraphConfig := host != "", false
	if graphEnabled {
		seconds, parseErr := strconv.ParseFloat(get("GRAPHIFY_DOCKER_REQUEST_TIMEOUT_SECONDS", "10"), 64)
		if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 ||
			seconds > float64(math.MaxInt64)/float64(time.Second) {
			graphEnabled, invalidGraphConfig = false, true
		} else {
			duration := time.Duration(seconds * float64(time.Second))
			if duration <= 0 {
				graphEnabled, invalidGraphConfig = false, true
			} else {
				graphTimeout = duration
			}
		}
		if graphEnabled {
			if _, hostErr := NewDockerClient(host, graphTimeout).base(); hostErr != nil {
				graphEnabled, invalidGraphConfig = false, true
			}
		}
	}
	return Config{GitHubUsername: username, GitHubToken: token, Branch: get("BRANCH", ""), RegistryURL: get("REGISTRY_URL", "registry.localdomain:5000"), StateFile: get("STATE_FILE", "/data/state.json"), PollIntervalSeconds: poll, BuildTimeoutSeconds: buildTimeout, HeartbeatFile: get("HEARTBEAT_FILE", "/data/heartbeat"), DockerHost: host, DockerBuildkit: get("DOCKER_BUILDKIT", "0"), GraphifyRequestTimeout: graphTimeout, GraphifyEnabled: graphEnabled, GraphifyInvalidConfig: invalidGraphConfig}, nil
}

const maxStatePathSymlinks = 255

type State map[string]any

func LoadState(path string) (State, error) {
	return loadState(path, syncStateDirectory)
}

func loadState(path string, syncDirectory func(string) error) (State, error) {
	path, err := normalizeStatePath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := ensureStateDirectory(filepath.Dir(path), syncDirectory); err != nil {
			return nil, err
		}
		return State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("state must contain one JSON value")
		}
		return nil, err
	}
	if state == nil {
		return nil, errors.New("state must be a JSON object")
	}
	if err := syncDirectoryAncestry(filepath.Dir(path), syncDirectory); err != nil {
		return nil, err
	}
	return state, nil
}

func SaveState(path string, state State) error {
	return saveState(path, state, syncStateDirectory)
}

func saveState(path string, state State, syncDirectory func(string) error) error {
	path, err := normalizeStatePath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureStateDirectory(dir, syncDirectory); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err = file.Chmod(0o600); err == nil {
		encoder := json.NewEncoder(file)
		err = encoder.Encode(state)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return nil
}

func ensureStateDirectory(directory string, syncDirectory func(string) error) error {
	directory = filepath.Clean(directory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return syncDirectoryAncestry(directory, syncDirectory)
}

func syncDirectoryAncestry(directory string, syncDirectory func(string) error) error {
	absolute, err := filepath.Abs(filepath.Clean(directory))
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	symlinkParents, err := directorySymlinkParents(absolute)
	if err != nil {
		return err
	}
	if err := syncPathAncestry(absolute, syncDirectory); err != nil {
		return err
	}
	for _, parent := range symlinkParents {
		if err := syncPathAncestry(parent, syncDirectory); err != nil {
			return err
		}
	}
	if resolved != absolute {
		return syncPathAncestry(resolved, syncDirectory)
	}
	return nil
}

func directorySymlinkParents(directory string) ([]string, error) {
	volume := filepath.VolumeName(directory)
	root := volume + string(filepath.Separator)
	current := root
	remainder := strings.TrimPrefix(directory, root)
	components := strings.Split(remainder, string(filepath.Separator))
	parents := make([]string, 0, 2)
	symlinkCount := 0
	for len(components) > 0 {
		component := components[0]
		components = components[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		symlinkCount++
		if symlinkCount > maxStatePathSymlinks {
			return nil, errors.New("too many state-directory symlinks")
		}
		parents = append(parents, current)
		target, err := os.Readlink(next)
		if err != nil {
			return nil, err
		}
		if filepath.IsAbs(target) {
			targetVolume := filepath.VolumeName(target)
			current = targetVolume + string(filepath.Separator)
			target = strings.TrimPrefix(target, targetVolume)
			target = strings.TrimLeft(target, string(filepath.Separator))
		}
		components = append(strings.Split(target, string(filepath.Separator)), components...)
	}
	return parents, nil
}

func syncPathAncestry(directory string, syncDirectory func(string) error) error {
	ancestry := make([]string, 0, 4)
	for current := directory; ; current = filepath.Dir(current) {
		ancestry = append(ancestry, current)
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for index := len(ancestry) - 1; index >= 0; index-- {
		if err := syncDirectory(ancestry[index]); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStatePath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("state file must not be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return absolute, nil
}

func syncStateDirectory(directory string) error {
	dirFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func StateEntry(state State, name string) (string, string) {
	value, exists := state[name]
	if !exists || value == nil {
		return "", ""
	}
	if sha, ok := value.(string); ok {
		return sha, ""
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return "", ""
	}
	sha, _ := entry["sha"].(string)
	pushedAt, _ := entry["pushed_at"].(string)
	return sha, pushedAt
}

func WriteLog(out io.Writer, level, event string, fields map[string]any) {
	record := map[string]any{"level": level, "event": event}
	for key, value := range fields {
		switch key {
		case "repo", "branch", "sha", "image", "count", "status", "url", "error_type", "exit_code", "output_tail", "outcome", "not_persisted", "reason", "exec_id", "retry_delay", "repos", "diagnostic_class":
			record[key] = value
		}
	}
	_ = json.NewEncoder(out).Encode(record)
}
