package watcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const MaxBuildOutputChars = 4000

type BuildRequest struct {
	CloneURL       string
	Branch         string
	Token          string
	RegistryURL    string
	Image          string
	DockerBuildkit string
	Timeout        time.Duration
}

type BuildFailure struct {
	Event      string `json:"event"`
	ExitCode   any    `json:"exit_code"`
	OutputTail string `json:"output_tail,omitempty"`
}

type buildError struct {
	code   any
	output string
	cause  error
}

func (e *buildError) Error() string { return "build failed" }
func (e *buildError) Unwrap() error { return e.cause }

func RenderBuildFailure(err error) BuildFailure {
	var failure *buildError
	if !errors.As(err, &failure) {
		return BuildFailure{Event: "build failed", ExitCode: "error"}
	}
	return BuildFailure{Event: "build failed", ExitCode: failure.code, OutputTail: tail(failure.output, MaxBuildOutputChars)}
}

type tailWriter struct {
	data  []byte
	limit int
}

func newTailWriter(limit int) *tailWriter {
	if limit < 0 {
		limit = 0
	}
	return &tailWriter{data: make([]byte, 0, limit), limit: limit}
}

func (w *tailWriter) Write(data []byte) (int, error) {
	written := len(data)
	if w.limit == 0 {
		return written, nil
	}
	if len(data) >= w.limit {
		w.data = append(w.data[:0], data[len(data)-w.limit:]...)
		return written, nil
	}
	excess := len(w.data) + len(data) - w.limit
	if excess > 0 {
		copy(w.data, w.data[excess:])
		w.data = w.data[:len(w.data)-excess]
	}
	w.data = append(w.data, data...)
	return written, nil
}

func tail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[len(runes)-limit:]
	}
	return string(runes)
}

func BuildAndPush(ctx context.Context, request BuildRequest, onBuilt func(string) error) (string, error) {
	workdir, err := os.MkdirTemp("", "docker-watcher-")
	if err != nil {
		return "", fmt.Errorf("create build workspace")
	}
	defer os.RemoveAll(workdir)
	credentialDir, err := os.MkdirTemp("", "docker-watcher-git-credential-")
	if err != nil {
		return "", fmt.Errorf("git clone failed")
	}
	defer os.RemoveAll(credentialDir)
	if err := os.Chmod(credentialDir, 0o700); err != nil {
		return "", fmt.Errorf("git clone failed")
	}
	credential, err := gitCredential(request.CloneURL, request.Token)
	if err != nil {
		return "", fmt.Errorf("git clone failed")
	}
	credentialPath := filepath.Join(credentialDir, "credentials")
	if err := os.WriteFile(credentialPath, []byte(credential), 0o600); err != nil {
		return "", fmt.Errorf("git clone failed")
	}
	if err := os.Chmod(credentialPath, 0o600); err != nil {
		return "", fmt.Errorf("git clone failed")
	}
	helper := "credential.helper=store --file=" + credentialPath
	if err := runQuiet(ctx, request.Timeout, workdir, nil, "git", "-c", helper, "clone", "--depth", "1", "--branch", request.Branch, request.CloneURL, workdir); err != nil {
		return "", &buildError{code: buildExitCode(err), cause: err}
	}
	if err := os.RemoveAll(credentialDir); err != nil {
		return "", fmt.Errorf("remove Git credentials")
	}
	var shaOutput bytes.Buffer
	if err := runCaptured(ctx, request.Timeout, workdir, nil, &shaOutput, "git", "rev-parse", "HEAD"); err != nil {
		return "", &buildError{code: buildExitCode(err), cause: err}
	}
	sha := strings.TrimSpace(shaOutput.String())
	if sha == "" {
		return "", fmt.Errorf("git revision failed")
	}
	if err := os.RemoveAll(filepath.Join(workdir, ".git")); err != nil {
		return "", fmt.Errorf("remove git metadata")
	}
	base := strings.TrimRight(request.RegistryURL, "/") + "/" + strings.TrimLeft(request.Image, "/")
	tags := []string{base + ":latest", base + ":" + shortSHA(sha)}
	args := []string{"build"}
	for _, tag := range tags {
		args = append(args, "-t", tag)
	}
	args = append(args, workdir)
	buildkit := request.DockerBuildkit
	if buildkit == "" {
		buildkit = "0"
	}
	output, err := runOutputWithLimit(ctx, request.Timeout, workdir, []string{"DOCKER_BUILDKIT=" + buildkit}, MaxBuildOutputChars*4+len(request.Token), "docker", args...)
	if err != nil {
		return "", &buildError{code: buildExitCode(err), output: redact(output, request.Token), cause: err}
	}
	if onBuilt != nil {
		if err := onBuilt(sha); err != nil {
			return "", fmt.Errorf("persist build target")
		}
	}
	for _, tag := range tags {
		output, err = runOutputWithLimit(ctx, request.Timeout, workdir, []string{"DOCKER_BUILDKIT=" + buildkit}, MaxBuildOutputChars*4+len(request.Token), "docker", "push", tag)
		if err != nil {
			return "", &buildError{code: buildExitCode(err), output: redact(output, request.Token), cause: err}
		}
	}
	return sha, nil
}

func gitCredential(cloneURL, token string) (string, error) {
	parsed, err := url.Parse(cloneURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(cloneURL, "#") || token == "" {
		return "", errors.New("invalid Git credential request")
	}
	credentialURL := url.URL{
		Scheme: "https",
		User:   url.UserPassword("x-access-token", token),
		Host:   "github.com",
	}
	return credentialURL.String() + "\n", nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
func redact(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}

func buildExitCode(err error) any {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return "error"
}

func commandContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	return context.WithTimeout(parent, timeout)
}

const commandPipeWaitDelay = 250 * time.Millisecond

func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = commandPipeWaitDelay
	cmd.Cancel = func() error { return terminateProcessGroup(cmd) }
}

func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func runQuiet(parent context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) error {
	ctx, cancel := commandContext(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = mergedEnvironment(env)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err := cmd.Run()
	_ = terminateProcessGroup(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func runCaptured(parent context.Context, timeout time.Duration, dir string, env []string, out io.Writer, name string, args ...string) error {
	ctx, cancel := commandContext(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = mergedEnvironment(env)
	cmd.Stdout, cmd.Stderr = out, io.Discard
	err := cmd.Run()
	_ = terminateProcessGroup(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func runOutputWithLimit(parent context.Context, timeout time.Duration, dir string, env []string, limit int, name string, args ...string) (string, error) {
	ctx, cancel := commandContext(parent, timeout)
	defer cancel()
	stdout, stderr := newTailWriter(limit), newTailWriter(limit)
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = mergedEnvironment(env)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	_ = terminateProcessGroup(cmd)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return string(stdout.data) + string(stderr.data), err
}

func mergedEnvironment(overrides []string) []string {
	replacements := make(map[string]string, len(overrides))
	for _, item := range overrides {
		key, _, ok := strings.Cut(item, "=")
		if ok && key != "GITHUB_TOKEN" {
			replacements[key] = item
		}
	}
	current := os.Environ()
	result := make([]string, 0, len(current)+len(replacements))
	for _, item := range current {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "GITHUB_TOKEN" {
			continue
		}
		if _, replaced := replacements[key]; !replaced {
			result = append(result, item)
		}
	}
	for _, item := range overrides {
		key, _, ok := strings.Cut(item, "=")
		if ok && key != "GITHUB_TOKEN" {
			if replacement, exists := replacements[key]; exists {
				result = append(result, replacement)
				delete(replacements, key)
			}
		}
	}
	return result
}
