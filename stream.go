package watcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MaxFrameBytes = 1_048_576
const ScanCapBytes = 65_536
const streamHeaderCap = 65_536
const streamReadSize = 8192

type FrameParser struct {
	header    []byte
	remaining uint32
	stream    byte
	payload   int
	stderr    []byte
	invalid   bool
	capped    bool
}

func NewFrameParser() *FrameParser { return &FrameParser{} }
func (p *FrameParser) Feed(data []byte) error {
	offset := 0
	for offset < len(data) && !p.invalid && !p.capped {
		if p.remaining == 0 {
			need := 8 - len(p.header)
			take := min(need, len(data)-offset)
			p.header = append(p.header, data[offset:offset+take]...)
			offset += take
			if len(p.header) != 8 {
				continue
			}
			frameType, size := p.header[0], binary.BigEndian.Uint32(p.header[4:])
			reserved := p.header[1:4]
			p.header = p.header[:0]
			if (frameType != 1 && frameType != 2) || reserved[0] != 0 || reserved[1] != 0 || reserved[2] != 0 || size > MaxFrameBytes {
				p.invalid = true
				return errors.New("invalid Docker stream frame")
			}
			p.stream, p.remaining = frameType, size
			if size == 0 {
				continue
			}
		}
		take := min(int(p.remaining), len(data)-offset)
		p.payload += take
		if p.payload > ScanCapBytes {
			p.capped = true
			return errors.New("Docker stream scan cap exceeded")
		}
		if p.stream == 2 {
			p.stderr = append(p.stderr, data[offset:offset+take]...)
		}
		p.remaining -= uint32(take)
		offset += take
	}
	return nil
}
func (p *FrameParser) Write(data []byte) (int, error) {
	if p.capped {
		return len(data), nil
	}
	err := p.Feed(data)
	if err != nil {
		if p.capped {
			return len(data), nil
		}
		return 0, err
	}
	return len(data), nil
}

func (p *FrameParser) Finish() string {
	if p.invalid || p.capped || p.remaining != 0 || len(p.header) != 0 {
		return "unavailable"
	}
	lines := bytes.Split(p.stderr, []byte{'\n'})
	for _, line := range lines {
		if bytes.HasSuffix(line, []byte{'\r'}) {
			line = line[:len(line)-1]
		}
		if bytes.HasPrefix(line, []byte("error: path not found: ")) {
			return "workspace_path_missing"
		}
	}
	signature := []byte("Nothing to update or rebuild failed — check output above.")
	for _, line := range lines {
		if bytes.HasSuffix(line, []byte{'\r'}) {
			line = line[:len(line)-1]
		}
		if bytes.Equal(line, signature) {
			return "graphify_rebuild_failed"
		}
	}
	return "unclassified"
}

type AttachedStream struct {
	host    string
	timeout time.Duration
}

func NewAttachedStream(host string, timeout time.Duration) *AttachedStream {
	return &AttachedStream{host: host, timeout: timeout}
}

func (s *AttachedStream) Start(ctx context.Context, requestPath string) (io.ReadCloser, error) {
	if s.timeout <= 0 {
		return nil, errors.New("Docker stream unavailable")
	}
	var network, address, hostHeader string
	if strings.HasPrefix(s.host, "tcp://") {
		address = strings.TrimPrefix(s.host, "tcp://")
		if _, _, err := net.SplitHostPort(address); err != nil {
			return nil, errors.New("Docker stream unavailable")
		}
		network, hostHeader = "tcp", address
	} else if strings.HasPrefix(s.host, "unix://") {
		address = strings.TrimPrefix(s.host, "unix://")
		if address == "" {
			return nil, errors.New("Docker stream unavailable")
		}
		network, hostHeader = "unix", "localhost"
	} else {
		return nil, errors.New("Docker stream unavailable")
	}
	setupCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: s.timeout}).DialContext(setupCtx, network, address)
	if err != nil {
		return nil, errors.New("Docker stream unavailable")
	}
	closeOnError := true
	cancelSignal := make(chan struct{})
	signalOnce := &sync.Once{}
	stopCancelWatch := func() { signalOnce.Do(func() { close(cancelSignal) }) }
	defer func() {
		if closeOnError {
			stopCancelWatch()
			_ = conn.Close()
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-cancelSignal:
		}
	}()
	if deadline, ok := setupCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	body := `{"Detach":false,"Tty":false}`
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", requestPath, hostHeader, len(body), body)
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, errors.New("Docker stream unavailable")
	}
	reader := bufio.NewReaderSize(conn, streamReadSize)
	status, headers, err := readResponseHeaders(reader)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			return nil, errors.New("Docker stream unavailable")
		}
		return nil, &DockerAPIError{StatusCode: status}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.New("Docker stream unavailable")
	}
	closeOnError = false
	return &framedBody{conn: conn, reader: reader, chunked: strings.EqualFold(headers.Get("Transfer-Encoding"), "chunked"), remaining: contentLength(headers), closeSignal: cancelSignal, closeOnce: signalOnce}, nil
}

func readResponseHeaders(reader *bufio.Reader) (int, http.Header, error) {
	var raw []byte
	for !bytes.Contains(raw, []byte("\r\n\r\n")) {
		if len(raw) > streamHeaderCap {
			return 0, nil, errors.New("headers too large")
		}
		part, err := reader.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		raw = append(raw, part)
	}
	lines := strings.Split(string(raw[:len(raw)-4]), "\r\n")
	if len(lines) == 0 {
		return 0, nil, errors.New("invalid response")
	}
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) != 3 || (parts[0] != "HTTP/1.0" && parts[0] != "HTTP/1.1") || len(parts[1]) != 3 {
		return 0, nil, errors.New("invalid status")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil || status < 100 || status > 599 || !validHeaderValue(parts[2]) {
		return 0, nil, errors.New("invalid status")
	}
	headers := make(http.Header)
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !validHeaderName(name) || !validHeaderValue(value) {
			return 0, nil, errors.New("invalid header")
		}
		headers.Add(http.CanonicalHeaderKey(name), strings.Trim(value, " \t"))
	}
	transfer := headers.Values("Transfer-Encoding")
	lengths := headers.Values("Content-Length")
	if len(transfer) > 1 || (len(transfer) == 1 && !strings.EqualFold(transfer[0], "chunked")) {
		return 0, nil, errors.New("unsupported transfer encoding")
	}
	if len(lengths) > 1 || (len(transfer) != 0 && len(lengths) != 0) {
		return 0, nil, errors.New("conflicting framing")
	}
	if len(lengths) == 1 {
		if lengths[0] == "" {
			return 0, nil, errors.New("invalid content length")
		}
		for _, digit := range lengths[0] {
			if digit < '0' || digit > '9' {
				return 0, nil, errors.New("invalid content length")
			}
		}
		if _, err := strconv.ParseInt(lengths[0], 10, 64); err != nil {
			return 0, nil, errors.New("invalid content length")
		}
	}
	return status, headers, nil
}
func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}
func validHeaderValue(value string) bool {
	for _, char := range value {
		if char != '\t' && (char < ' ' || char == 127) {
			return false
		}
	}
	return true
}
func contentLength(headers http.Header) int64 {
	raw := headers.Get("Content-Length")
	if raw == "" {
		return -1
	}
	size, _ := strconv.ParseInt(raw, 10, 64)
	return size
}

type framedBody struct {
	conn           net.Conn
	reader         *bufio.Reader
	chunked        bool
	remaining      int64
	chunkRemaining int64
	done           bool
	mu             sync.Mutex
	closeSignal    chan struct{}
	closeOnce      *sync.Once
}

func (b *framedBody) Read(dst []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return 0, io.EOF
	}
	if len(dst) == 0 {
		return 0, nil
	}
	if b.chunked {
		return b.readChunk(dst)
	}
	if b.remaining == 0 {
		b.done = true
		return 0, io.EOF
	}
	if b.remaining > 0 && int64(len(dst)) > b.remaining {
		dst = dst[:b.remaining]
	}
	n, err := b.reader.Read(dst)
	if n > 0 && b.remaining > 0 {
		b.remaining -= int64(n)
	}
	if err == io.EOF && b.remaining > 0 {
		return n, errors.New("truncated Docker body")
	}
	return n, err
}
func (b *framedBody) readChunk(dst []byte) (int, error) {
	for {
		if b.chunkRemaining == 0 {
			line, err := readLimitedLine(b.reader, 128)
			if err != nil || !strings.HasSuffix(line, "\r\n") {
				return 0, errors.New("invalid chunk")
			}
			sizeText := strings.SplitN(strings.TrimSuffix(line, "\r\n"), ";", 2)[0]
			if sizeText == "" {
				return 0, errors.New("invalid chunk")
			}
			for _, digit := range sizeText {
				if !strings.ContainsRune("0123456789abcdefABCDEF", digit) {
					return 0, errors.New("invalid chunk")
				}
			}
			size, err := strconv.ParseInt(sizeText, 16, 64)
			if err != nil || size < 0 {
				return 0, errors.New("invalid chunk")
			}
			if size == 0 {
				if err := readTrailers(b.reader); err != nil {
					return 0, err
				}
				b.done = true
				return 0, io.EOF
			}
			b.chunkRemaining = size
		}
		if int64(len(dst)) > b.chunkRemaining {
			dst = dst[:b.chunkRemaining]
		}
		n, err := io.ReadFull(b.reader, dst)
		b.chunkRemaining -= int64(n)
		if err != nil {
			return n, errors.New("truncated Docker chunk")
		}
		if b.chunkRemaining == 0 {
			ending := make([]byte, 2)
			if _, err := io.ReadFull(b.reader, ending); err != nil || !bytes.Equal(ending, []byte("\r\n")) {
				return n, errors.New("invalid chunk delimiter")
			}
		}
		return n, nil
	}
}
func readLimitedLine(reader *bufio.Reader, limit int) (string, error) {
	line := make([]byte, 0, min(limit, streamReadSize))
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return "", errors.New("line exceeds limit")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return "", err
		}
		return string(line), nil
	}
}
func readTrailers(reader *bufio.Reader) error {
	total := 0
	for {
		line, err := readLimitedLine(reader, streamHeaderCap)
		total += len(line)
		if err != nil || total > streamHeaderCap || !strings.HasSuffix(line, "\r\n") {
			return errors.New("invalid trailers")
		}
		field := strings.TrimSuffix(line, "\r\n")
		if field == "" {
			return nil
		}
		name, value, ok := strings.Cut(field, ":")
		if !ok || !validHeaderName(name) || !validHeaderValue(value) || strings.EqualFold(name, "content-length") || strings.EqualFold(name, "transfer-encoding") {
			return errors.New("invalid trailers")
		}
	}
}
func (b *framedBody) Close() error {
	b.closeOnce.Do(func() { close(b.closeSignal) })
	err := b.conn.Close()
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	return err
}
