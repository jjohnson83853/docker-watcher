package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDockerEngineNegotiatesVersionAndPreservesGraphifyExecContract(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/graphify-mcp/json":
			_, _ = w.Write([]byte(`{"Id":"aaaaaaaaaaaa"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/aaaaaaaaaaaa/exec":
			var body struct {
				AttachStdout bool     `json:"AttachStdout"`
				AttachStderr bool     `json:"AttachStderr"`
				Tty          bool     `json:"Tty"`
				Cmd          []string `json:"Cmd"`
				Env          []string `json:"Env"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("invalid exec request: %v", err)
			}
			if !body.AttachStdout || !body.AttachStderr || body.Tty || !equalStrings(body.Cmd, []string{"graphify", "update", "/workspace"}) || !equalStrings(body.Env, []string{"GRAPHIFY_OUT=/data"}) {
				t.Errorf("Graphify exec contract changed: %+v", body)
			}
			_, _ = w.Write([]byte(`{"Id":"bbbbbbbbbbbb"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/exec/bbbbbbbbbbbb/json":
			_, _ = w.Write([]byte(`{"Running":false,"ExitCode":0}`))
		default:
			http.Error(w, "unrecognized engine request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
	containerID, err := client.ContainerID(context.Background())
	if err != nil || containerID != "aaaaaaaaaaaa" {
		t.Fatalf("ContainerID = %q, %v", containerID, err)
	}
	execID, err := client.ExecCreate(context.Background(), containerID, []string{"graphify", "update", "/workspace"}, []string{"GRAPHIFY_OUT=/data"})
	if err != nil || execID != "bbbbbbbbbbbb" {
		t.Fatalf("ExecCreate = %q, %v", execID, err)
	}
	inspection, err := client.ExecInspect(context.Background(), execID)
	if err != nil || inspection.Running || inspection.ExitCode == nil || *inspection.ExitCode != 0 {
		t.Fatalf("ExecInspect = %+v, %v", inspection, err)
	}
	want := []string{"GET /version", "GET /v1.44/containers/graphify-mcp/json", "POST /v1.44/containers/aaaaaaaaaaaa/exec", "GET /v1.44/exec/bbbbbbbbbbbb/json"}
	if !equalStrings(requests, want) {
		t.Fatalf("Docker API negotiation/request order changed: %v", requests)
	}
}

func TestDockerEngineRejectsUnsupportedHostAndMalformedVersion(t *testing.T) {
	for _, host := range []string{"http://localhost:2375", "unix://"} {
		client := NewDockerClient(host, time.Second)
		if _, err := client.ContainerID(context.Background()); err == nil {
			t.Errorf("unsupported Docker endpoint accepted: %q", host)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ApiVersion":"latest"}`))
	}))
	defer server.Close()
	client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
	if _, err := client.ContainerID(context.Background()); err == nil {
		t.Fatal("malformed API version accepted")
	}
}

func TestDockerEngineErrorsDoNotExposeResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private daemon detail /srv/customer", http.StatusForbidden)
	}))
	defer server.Close()
	client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
	_, err := client.ContainerID(context.Background())
	if err == nil || strings.Contains(err.Error(), "private daemon detail") || strings.Contains(err.Error(), "/srv/customer") {
		t.Fatalf("raw Docker response leaked through error: %v", err)
	}
}
