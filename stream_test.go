package watcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFrameParserDoesNotClassifySignatureSeenOnlyOnStdout(t *testing.T) {
	parser := NewFrameParser()
	if err := parser.Feed(streamFrame(1, []byte("error: path not found: /private/path"))); err != nil {
		t.Fatal(err)
	}
	if got := parser.Finish(); got != "unclassified" {
		t.Fatalf("stdout-only diagnostic = %q, want unclassified", got)
	}
}

func TestFrameParserHandlesFragmentedMultipleFramesAndStderrOnly(t *testing.T) {
	wire := append(streamFrame(1, []byte("error: path not found: stdout-only")), streamFrame(2, []byte("ordinary diagnostic\nerror: path not found: /private/workspace"))...)
	parser := NewFrameParser()
	for _, chunk := range wire {
		if err := parser.Feed([]byte{chunk}); err != nil {
			t.Fatal(err)
		}
	}
	if got := parser.Finish(); got != "workspace_path_missing" {
		t.Fatalf("fragmented stderr diagnostic = %q", got)
	}
}

func TestFrameParserEnforcesFrameAndAggregateCaps(t *testing.T) {
	oversizedHeader := make([]byte, 8)
	oversizedHeader[0] = 2
	binary.BigEndian.PutUint32(oversizedHeader[4:], MaxFrameBytes+1)
	parser := NewFrameParser()
	if err := parser.Feed(oversizedHeader); err == nil || parser.Finish() != "unavailable" {
		t.Fatalf("oversized frame was accepted: err=%v result=%q", err, parser.Finish())
	}
	parser = NewFrameParser()
	if err := parser.Feed(streamFrame(1, bytes.Repeat([]byte{'x'}, ScanCapBytes))); err != nil {
		t.Fatal(err)
	}
	if got := parser.Finish(); got != "unclassified" {
		t.Fatalf("exact scan-cap payload should remain classifiable: %q", got)
	}
	parser = NewFrameParser()
	wire := append(streamFrame(1, bytes.Repeat([]byte{'x'}, ScanCapBytes)), streamFrame(2, []byte{'y'})...)
	if err := parser.Feed(wire); err == nil || parser.Finish() != "unavailable" {
		t.Fatalf("aggregate cap across stdout/stderr not enforced: err=%v result=%q", err, parser.Finish())
	}
}

func TestFrameParserRejectsMalformedTruncatedAndUnknownFrameTypes(t *testing.T) {
	for name, wire := range map[string][]byte{
		"unknown type":      {3, 0, 0, 0, 0, 0, 0, 0},
		"reserved bytes":    {2, 1, 0, 0, 0, 0, 0, 0},
		"truncated header":  {2, 0, 0},
		"truncated payload": streamFrame(2, []byte("partial"))[:12],
	} {
		t.Run(name, func(t *testing.T) {
			parser := NewFrameParser()
			_ = parser.Feed(wire)
			if got := parser.Finish(); got != "unavailable" {
				t.Fatalf("malformed stream classified as %q", got)
			}
		})
	}
}

func TestFrameParserUsesFixedDiagnosticClassesWithoutReturningPayload(t *testing.T) {
	for input, want := range map[string]string{
		"error: path not found: /private/path\n":                    "workspace_path_missing",
		"Nothing to update or rebuild failed — check output above.": "graphify_rebuild_failed",
		"arbitrary private output":                                  "unclassified",
	} {
		parser := NewFrameParser()
		if err := parser.Feed(streamFrame(2, []byte(input))); err != nil {
			t.Fatal(err)
		}
		if got := parser.Finish(); got != want || strings.Contains(got, "/private") {
			t.Fatalf("diagnostic class = %q, want fixed class %q", got, want)
		}
	}
}

type consumedStreamReader struct {
	reader io.Reader
	read   int64
}

func (r *consumedStreamReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	return n, err
}

func TestFrameParserCopyConsumesStreamAfterScanCap(t *testing.T) {
	wire := streamFrame(2, bytes.Repeat([]byte{'x'}, ScanCapBytes+1))
	wire = append(wire, streamFrame(2, []byte("remaining stream"))...)
	source := &consumedStreamReader{reader: bytes.NewReader(wire)}
	parser := NewFrameParser()
	written, err := io.Copy(parser, source)
	if err != nil {
		t.Fatalf("io.Copy stopped when diagnostic scan cap was reached: %v", err)
	}
	if written != int64(len(wire)) || source.read != int64(len(wire)) {
		t.Fatalf("stream consumed %d bytes and copied %d of %d after scan cap", source.read, written, len(wire))
	}
	if got := parser.Finish(); got != "unavailable" {
		t.Fatalf("capped stream diagnostic = %q; want unavailable", got)
	}
}

func TestFrameParserCopyRejectsMalformedFramingBeforeScanCap(t *testing.T) {
	wire := append(streamFrame(2, []byte("small")), []byte{3, 0, 0, 0, 0, 0, 0, 0}...)
	parser := NewFrameParser()
	if _, err := io.Copy(parser, bytes.NewReader(wire)); err == nil {
		t.Fatal("io.Copy accepted malformed framing before reaching the scan cap")
	}
	if got := parser.Finish(); got != "unavailable" {
		t.Fatalf("malformed stream diagnostic = %q; want unavailable", got)
	}
}

func TestAttachedStreamOverTCPHandlesFragmentedChunkedBody(t *testing.T) {
	frame := streamFrame(2, []byte("error: path not found: /private/path"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.44/exec/exec-id/start" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, b := range frame {
			_, _ = w.Write([]byte{b})
			flusher.Flush()
		}
	}))
	defer server.Close()
	body, err := NewAttachedStream("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second).Start(context.Background(), "/v1.44/exec/exec-id/start")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	parser := NewFrameParser()
	if _, err := io.Copy(parser, body); err != nil {
		t.Fatal(err)
	}
	if got := parser.Finish(); got != "workspace_path_missing" {
		t.Fatalf("chunked fragmented stream classification = %q", got)
	}
}

func TestAttachedStreamReadsContentLengthFramedBody(t *testing.T) {
	frame := streamFrame(2, []byte("ordinary diagnostic"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame)
	}))
	defer server.Close()
	body, err := NewAttachedStream("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second).Start(context.Background(), "/v1.44/exec/exec-id/start")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	parser := NewFrameParser()
	if _, err := io.Copy(parser, body); err != nil {
		t.Fatal(err)
	}
	if got := parser.Finish(); got != "unclassified" {
		t.Fatalf("content-length response classification = %q", got)
	}
}

func TestAttachedStreamUnixSocketAndRejectsHTTPFailure(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "engine.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1.44/exec/exec-id/start") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "sensitive daemon detail", http.StatusForbidden)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	body, err := NewAttachedStream("unix://"+socketPath, time.Second).Start(context.Background(), "/v1.44/exec/exec-id/start")
	if err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("non-success Docker response accepted")
	}
	if strings.Contains(err.Error(), "sensitive daemon detail") {
		t.Fatalf("HTTP response body leaked through error: %v", err)
	}
}

func TestAttachedStreamCancellationClosesBlockedResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer func() { close(release); server.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		body, err := NewAttachedStream("tcp://"+strings.TrimPrefix(server.URL, "http://"), time.Second).Start(ctx, "/v1.44/exec/exec-id/start")
		if body != nil {
			body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach local peer")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled attach unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled attach did not return promptly")
	}
}

func streamFrame(stream byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

func TestAttachedStreamBoundsHeaderSetupWithoutExternalPeer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer func() { close(release); server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		body, err := NewAttachedStream("tcp://"+strings.TrimPrefix(server.URL, "http://"), 25*time.Millisecond).Start(ctx, "/v1.44/exec/exec-id/start")
		if body != nil {
			body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("header-stalling request did not reach scripted local peer")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled Docker response headers exceeded setup bound without error")
		}
	case <-time.After(time.Second):
		t.Fatal("Docker response header setup was not bounded")
	}
}
