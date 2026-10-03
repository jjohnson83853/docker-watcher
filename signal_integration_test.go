package watcher

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartSignalChild(t *testing.T) {
	if os.Getenv("WATCHER_SIGNAL_CHILD") != "1" {
		return
	}
	if err := Start(); err != nil {
		t.Fatal(err)
	}
}

func TestStartReturnsCleanlyOnSIGTERM(t *testing.T) {
	proxyRequest := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			select {
			case proxyRequest <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.Error(w, "unexpected proxy request", http.StatusBadGateway)
	}))
	defer proxy.Close()

	stateDir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestStartSignalChild$")
	command.Env = signalChildEnvironment(os.Environ(), map[string]string{
		"WATCHER_SIGNAL_CHILD":  "1",
		"GITHUB_USERNAME":       "local-owner",
		"GITHUB_TOKEN":          "local-test-token",
		"STATE_FILE":            filepath.Join(stateDir, "state.json"),
		"HEARTBEAT_FILE":        filepath.Join(stateDir, "heartbeat"),
		"POLL_INTERVAL_SECONDS": "60",
		"HTTPS_PROXY":           proxy.URL,
		"https_proxy":           proxy.URL,
		"NO_PROXY":              "",
		"no_proxy":              "",
		"ALL_PROXY":             "",
		"all_proxy":             "",
	})
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start local child process: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case <-proxyRequest:
	case err := <-waited:
		t.Fatalf("child exited before its local-only GitHub request: %v; output=%q", err, output.String())
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-waited
		t.Fatalf("child did not issue its proxied local request; output=%q", output.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to child: %v", err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("child did not stop cleanly after SIGTERM: %v; output=%q", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-waited
		t.Fatalf("child did not stop after SIGTERM; output=%q", output.String())
	}
}

func signalChildEnvironment(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
