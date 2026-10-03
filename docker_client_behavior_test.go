package watcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDockerExecStartClassifiesCompleteAndIncompleteFrames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		wantClass string
		wantErr   string
	}{
		{name: "ordinary output", body: streamFrame(2, []byte("routine output"))},
		{name: "workspace diagnostic", body: streamFrame(2, []byte("error: path not found: /private/workspace")), wantClass: "workspace_path_missing"},
		{name: "truncated frame", body: []byte{2, 0, 0, 0, 0, 0, 0, 4, 'x'}, wantErr: "Docker stream unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/version" {
					_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1.44/exec/abcdef123456/start" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)))
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
			err := client.ExecStart(context.Background(), "abcdef123456")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("ExecStart error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if tc.wantClass == "" {
				if err != nil {
					t.Fatalf("ExecStart ordinary response error = %v", err)
				}
				return
			}
			var diagnostic *captureDiagnosticError
			if !errors.As(err, &diagnostic) || diagnostic.class != tc.wantClass || err.Error() != "Docker stream diagnostic" {
				t.Fatalf("ExecStart diagnostic = %#v, %v", diagnostic, err)
			}
		})
	}
}

func TestDockerClientErrorsStayTypedAndSafe(t *testing.T) {
	unreachable := NewDockerClient("tcp://127.0.0.1:1", 30*time.Millisecond)
	if _, err := unreachable.ContainerID(context.Background()); err == nil || err.Error() != "docker unavailable" {
		t.Fatalf("unreachable Docker error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal daemon path /private", http.StatusForbidden)
	}))
	defer server.Close()
	client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
	_, err := client.ContainerID(context.Background())
	var apiError *DockerAPIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusForbidden || err.Error() != "docker api error" {
		t.Fatalf("Docker API error = %#v, %v", apiError, err)
	}
	if strings.Contains(err.Error(), "/private") {
		t.Fatalf("Docker API error leaked response details: %v", err)
	}

	for _, invalidID := range []string{"", "not an exec id"} {
		if _, err := client.ExecCreate(context.Background(), invalidID, nil, nil); err == nil {
			t.Errorf("ExecCreate accepted invalid container ID %q", invalidID)
		}
		if _, err := client.ExecInspect(context.Background(), invalidID); err == nil {
			t.Errorf("ExecInspect accepted invalid exec ID %q", invalidID)
		}
	}
}
