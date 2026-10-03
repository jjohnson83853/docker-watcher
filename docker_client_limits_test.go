package watcher

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

type countedResponseBody struct {
	remaining int
	read      int
	closed    bool
}

func (b *countedResponseBody) Read(buffer []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(buffer), b.remaining)
	for i := range buffer[:n] {
		buffer[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

func (b *countedResponseBody) Close() error {
	b.closed = true
	return nil
}

type responseRoundTripper func(*http.Request) (*http.Response, error)

func (f responseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDockerRequestBoundsSuccessfulResponseBody(t *testing.T) {
	const maxResponseBytes = 1 << 20
	body := &countedResponseBody{remaining: maxResponseBytes * 2}
	client := NewDockerClient("tcp://127.0.0.1:2375", time.Second)
	client.client = &http.Client{Transport: responseRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: request}, nil
	})}

	status, data, err := client.request(context.Background(), http.MethodGet, "/version", nil)
	if err == nil || status != http.StatusOK {
		t.Fatalf("oversized Docker response = status %d, %d bytes, err %v; want bounded failure", status, len(data), err)
	}
	if got, want := err.Error(), "Docker response too large"; got != want {
		t.Fatalf("oversized Docker response error = %q; want safe generic %q", got, want)
	}
	if body.read != maxResponseBytes+1 || len(data) > maxResponseBytes {
		t.Fatalf("oversized response read %d bytes and retained %d; want at most %d read bytes", body.read, len(data), maxResponseBytes+1)
	}
	if !body.closed {
		t.Fatal("successful Docker response body was not closed")
	}
}

func TestDockerRequestDoesNotBufferNonSuccessResponseBody(t *testing.T) {
	body := &countedResponseBody{remaining: 4 << 20}
	client := NewDockerClient("tcp://127.0.0.1:2375", time.Second)
	client.client = &http.Client{Transport: responseRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: body, Header: make(http.Header), Request: request}, nil
	})}

	status, data, err := client.request(context.Background(), http.MethodGet, "/version", nil)
	if status != http.StatusBadGateway || len(data) != 0 || err == nil {
		t.Fatalf("Docker error response = status %d, %d bytes, err %v", status, len(data), err)
	}
	if body.read != 0 {
		t.Fatalf("non-success Docker response buffered %d body bytes", body.read)
	}
	if !body.closed {
		t.Fatal("non-success Docker response body was not closed")
	}
}
