package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var dockerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
var apiVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

const maxDockerResponseBytes = 1 << 20

type DockerResponseTooLarge struct{}

func (DockerResponseTooLarge) Error() string { return "Docker response too large" }

type DockerAPIError struct{ StatusCode int }

func (e *DockerAPIError) Error() string { return "docker api error" }

type DockerUnreachable struct{}

func (DockerUnreachable) Error() string { return "docker unavailable" }

type captureDiagnosticError struct{ class string }

func (e *captureDiagnosticError) Error() string { return "Docker stream diagnostic" }

type ExecInspection struct {
	Running  bool
	ExitCode *int
}

type GraphifyClient interface {
	ContainerID(context.Context) (string, error)
	ExecCreate(context.Context, string, []string, []string) (string, error)
	ExecStart(context.Context, string) error
	ExecInspect(context.Context, string) (ExecInspection, error)
}

type DockerClient struct {
	host      string
	client    *http.Client
	timeout   time.Duration
	apiPrefix string
	stream    *AttachedStream
}

func NewDockerClient(host string, timeout time.Duration) *DockerClient {
	client := &http.Client{Timeout: timeout}
	if strings.HasPrefix(host, "unix://") {
		socket := strings.TrimPrefix(host, "unix://")
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		}}
		client = &http.Client{Transport: transport, Timeout: timeout}
	} else if strings.HasPrefix(host, "tcp://") {
		address := strings.TrimPrefix(host, "tcp://")
		if _, _, err := net.SplitHostPort(address); err != nil {
			client = &http.Client{Timeout: timeout}
		}
	} else {
		client = &http.Client{Timeout: timeout}
	}
	return &DockerClient{host: host, client: client, timeout: timeout, stream: NewAttachedStream(host, timeout)}
}

func (c *DockerClient) base() (string, error) {
	switch {
	case strings.HasPrefix(c.host, "tcp://"):
		address := strings.TrimPrefix(c.host, "tcp://")
		host, port, err := net.SplitHostPort(address)
		portNumber, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || portNumber < 1 || portNumber > 65535 {
			return "", errors.New("invalid Docker endpoint")
		}
		return "http://" + address, nil
	case strings.HasPrefix(c.host, "unix://"):
		if strings.TrimPrefix(c.host, "unix://") == "" {
			return "", errors.New("invalid Docker endpoint")
		}
		return "http://docker", nil
	default:
		return "", errors.New("unsupported Docker endpoint")
	}
}

func (c *DockerClient) request(ctx context.Context, method, path string, body any) (int, []byte, error) {
	base, err := c.base()
	if err != nil {
		return 0, nil, err
	}
	var payload io.Reader
	if body != nil {
		data, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return 0, nil, errors.New("invalid Docker request")
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return 0, nil, errors.New("Docker request failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, DockerUnreachable{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, &DockerAPIError{StatusCode: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDockerResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, DockerUnreachable{}
	}
	if len(data) > maxDockerResponseBytes {
		return resp.StatusCode, nil, DockerResponseTooLarge{}
	}
	return resp.StatusCode, data, nil
}

func (c *DockerClient) prefix(ctx context.Context) (string, error) {
	if c.apiPrefix != "" {
		return c.apiPrefix, nil
	}
	_, data, err := c.request(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var response struct {
		APIVersion string `json:"ApiVersion"`
	}
	if json.Unmarshal(data, &response) != nil || !apiVersionPattern.MatchString(response.APIVersion) {
		return "", &DockerAPIError{StatusCode: 0}
	}
	c.apiPrefix = "/v" + response.APIVersion
	return c.apiPrefix, nil
}

func (c *DockerClient) ContainerID(ctx context.Context) (string, error) {
	prefix, err := c.prefix(ctx)
	if err != nil {
		return "", err
	}
	_, data, err := c.request(ctx, http.MethodGet, prefix+"/containers/graphify-mcp/json", nil)
	if err != nil {
		return "", err
	}
	var response struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(data, &response) != nil || !dockerID.MatchString(response.ID) {
		return "", &DockerAPIError{StatusCode: 0}
	}
	return response.ID, nil
}

func (c *DockerClient) ExecCreate(ctx context.Context, containerID string, command, environment []string) (string, error) {
	prefix, err := c.prefix(ctx)
	if err != nil {
		return "", err
	}
	if !dockerID.MatchString(containerID) {
		return "", &DockerAPIError{StatusCode: 0}
	}
	body := map[string]any{"AttachStdin": false, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": command, "Env": environment}
	_, data, err := c.request(ctx, http.MethodPost, prefix+"/containers/"+containerID+"/exec", body)
	if err != nil {
		return "", err
	}
	var response struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(data, &response) != nil || !dockerID.MatchString(response.ID) {
		return "", &DockerAPIError{StatusCode: 0}
	}
	return response.ID, nil
}

func (c *DockerClient) ExecStart(ctx context.Context, execID string) error {
	if !dockerID.MatchString(execID) {
		return &DockerAPIError{StatusCode: 0}
	}
	prefix, err := c.prefix(ctx)
	if err != nil {
		return err
	}
	body, err := c.stream.Start(ctx, prefix+"/exec/"+execID+"/start")
	if err != nil {
		return err
	}
	defer body.Close()
	parser := NewFrameParser()
	if _, err := io.Copy(parser, body); err != nil {
		return errors.New("Docker stream unavailable")
	}
	diagnostic := parser.Finish()
	if diagnostic == "unavailable" {
		return errors.New("Docker stream unavailable")
	}
	if diagnostic != "unclassified" {
		return &captureDiagnosticError{class: diagnostic}
	}
	return nil
}

func (c *DockerClient) ExecInspect(ctx context.Context, execID string) (ExecInspection, error) {
	if !dockerID.MatchString(execID) {
		return ExecInspection{}, &DockerAPIError{StatusCode: 0}
	}
	prefix, err := c.prefix(ctx)
	if err != nil {
		return ExecInspection{}, err
	}
	_, data, err := c.request(ctx, http.MethodGet, prefix+"/exec/"+execID+"/json", nil)
	if err != nil {
		return ExecInspection{}, err
	}
	var response struct {
		Running  *bool           `json:"Running"`
		ExitCode json.RawMessage `json:"ExitCode"`
	}
	if json.Unmarshal(data, &response) != nil || response.Running == nil || len(response.ExitCode) == 0 {
		return ExecInspection{}, &DockerAPIError{StatusCode: 0}
	}
	var exitCode *int
	if !bytes.Equal(response.ExitCode, []byte("null")) {
		var code int
		if json.Unmarshal(response.ExitCode, &code) != nil || code < 0 || code > 255 {
			return ExecInspection{}, &DockerAPIError{StatusCode: 0}
		}
		exitCode = &code
	}
	return ExecInspection{Running: *response.Running, ExitCode: exitCode}, nil
}
