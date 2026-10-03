package watcher

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDockerExecStartReconciles409AndRedactsErrors(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/version" {
					_, _ = io.WriteString(w, `{"ApiVersion":"1.44"}`)
					return
				}
				http.Error(w, "private engine path /secret/tenant", status)
			}))
			defer server.Close()
			client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
			err := client.ExecStart(context.Background(), "bbbbbbbbbbbb")
			var apiErr *DockerAPIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
				t.Fatalf("ExecStart error = %v; want DockerAPIError status %d", err, status)
			}
			if strings.Contains(err.Error(), "private engine") || strings.Contains(err.Error(), "/secret") {
				t.Fatalf("ExecStart leaked daemon response: %v", err)
			}
		})
	}
}

func TestDockerAPIRejectsMalformedSuccessfulResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		call func(*DockerClient) error
	}{
		{"version", "/version", `{"ApiVersion":{}}`, func(c *DockerClient) error { _, err := c.ContainerID(context.Background()); return err }},
		{"container id", "/v1.44/containers/graphify-mcp/json", `{"Id":"not-an-id"}`, func(c *DockerClient) error { _, err := c.ContainerID(context.Background()); return err }},
		{"exec create id", "/v1.44/containers/aaaaaaaaaaaa/exec", `{"Id":"bad"}`, func(c *DockerClient) error {
			_, err := c.ExecCreate(context.Background(), "aaaaaaaaaaaa", nil, nil)
			return err
		}},
		{"inspect fields", "/v1.44/exec/bbbbbbbbbbbb/json", `{"Running":"yes","ExitCode":0}`, func(c *DockerClient) error { _, err := c.ExecInspect(context.Background(), "bbbbbbbbbbbb"); return err }},
		{"inspect out of range", "/v1.44/exec/bbbbbbbbbbbb/json", `{"Running":false,"ExitCode":256}`, func(c *DockerClient) error { _, err := c.ExecInspect(context.Background(), "bbbbbbbbbbbb"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/version" {
					_, _ = io.WriteString(w, `{"ApiVersion":"1.44"}`)
					return
				}
				if r.URL.Path != tc.path {
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := NewDockerClient("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second)
			if err := tc.call(client); err == nil {
				t.Fatal("malformed successful Docker response accepted")
			}
		})
	}
}

func TestAttachedStreamRejectsMalformedOrTruncatedHeaders(t *testing.T) {
	for _, response := range []string{
		"not-http\r\n\r\n",
		"HTTP/1.1 200 OK\r\nBad Header: x\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: nope\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nX: y\r\n",
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(response), "\r\n", "_"), func(t *testing.T) {
			body := strings.NewReader(response)
			_, _, err := readResponseHeaders(bufio.NewReader(body))
			if err == nil {
				t.Fatalf("accepted malformed response headers %q", response)
			}
		})
	}
}

func TestAttachedStreamRejectsMalformedAndTruncatedChunkBodies(t *testing.T) {
	for _, wire := range []string{"z\r\n", "3\r\nab\r\n", "1\r\naXX", "0\r\nContent-Length: 1\r\n\r\n"} {
		t.Run(strings.ReplaceAll(wire, "\r\n", "_"), func(t *testing.T) {
			serverSide, clientSide := net.Pipe()
			defer serverSide.Close()
			body := &framedBody{conn: clientSide, reader: bufio.NewReader(clientSide), chunked: true, closeSignal: make(chan struct{}), closeOnce: &sync.Once{}}
			go func() {
				_, _ = io.WriteString(serverSide, wire)
				_ = serverSide.Close()
			}()
			_, err := io.ReadAll(body)
			_ = body.Close()
			if err == nil {
				t.Fatalf("accepted malformed/truncated chunk body %q", wire)
			}
		})
	}
}

func TestAttachedStreamCloseUnblocksBodyReadAndReportsConnectFault(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	body := &framedBody{conn: clientSide, reader: bufio.NewReader(clientSide), remaining: -1, closeSignal: make(chan struct{}), closeOnce: &sync.Once{}}
	readDone := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); readDone <- err }()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("closed body read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("closing body did not unblock read")
	}
	_ = serverSide.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachedStream("tcp://"+address, time.Second).Start(context.Background(), "/start"); err == nil {
		t.Fatal("connection-refused Docker endpoint accepted")
	}
}
