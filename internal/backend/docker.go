package backend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultDockerHost is the usual location of the Docker daemon socket.
const DefaultDockerHost = "unix:///var/run/docker.sock"

// Docker starts and stops an existing container.
//
// napio manages a container you already created; it never creates, removes or
// templates one. Your Factorio deployment stays yours — napio only decides
// when it runs.
//
// The official Docker SDK is not used on purpose: this backend needs exactly
// three endpoints, and the SDK would multiply napio's dependency tree and
// binary size for no benefit on a NAS or a Raspberry Pi.
type Docker struct {
	container string
	host      string
	client    *http.Client
	log       *slog.Logger
}

// NewDocker builds a Docker backend. host may be empty (meaning the default
// unix socket), "unix:///path/to.sock" or "tcp://host:port".
func NewDocker(container, host string, log *slog.Logger) (*Docker, error) {
	if container == "" {
		return nil, fmt.Errorf("docker backend: container name is required")
	}
	if host == "" {
		host = DefaultDockerHost
	}
	client, err := dockerClient(host)
	if err != nil {
		return nil, err
	}
	return &Docker{container: container, host: host, client: client, log: log}, nil
}

func dockerClient(host string) (*http.Client, error) {
	switch {
	case strings.HasPrefix(host, "unix://"):
		socket := strings.TrimPrefix(host, "unix://")
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		}, nil
	case strings.HasPrefix(host, "tcp://"):
		return &http.Client{}, nil
	default:
		return nil, fmt.Errorf("docker backend: unsupported host %q (use unix:// or tcp://)", host)
	}
}

func (d *Docker) Describe() string { return "docker container " + d.container }

// baseURL is a placeholder host for unix sockets; net/http still needs a URL.
func (d *Docker) url(path string, query url.Values) string {
	base := "http://docker"
	if strings.HasPrefix(d.host, "tcp://") {
		base = "http://" + strings.TrimPrefix(d.host, "tcp://")
	}
	u := base + "/v1.41" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (d *Docker) do(ctx context.Context, method, path string, query url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.url(path, query), nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

type containerInspect struct {
	State struct {
		Running    bool   `json:"Running"`
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

func (d *Docker) inspect(ctx context.Context) (*containerInspect, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+d.container+"/json", nil)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		var out containerInspect
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("docker: decoding inspect response: %w", err)
		}
		return &out, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("docker: container %q does not exist", d.container)
	default:
		return nil, fmt.Errorf("docker: inspect returned %d: %s", code, dockerMessage(body))
	}
}

func (d *Docker) IsRunning(ctx context.Context) (bool, error) {
	info, err := d.inspect(ctx)
	if err != nil {
		return false, err
	}
	return info.State.Running, nil
}

func (d *Docker) Start(ctx context.Context) error {
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+d.container+"/start", nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent:
		return nil
	case http.StatusNotModified: // already running
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("docker: container %q does not exist", d.container)
	default:
		// The most common cause here is the port handover going wrong: napio
		// still holds the UDP port the container wants to publish.
		return fmt.Errorf("docker: starting %q returned %d: %s", d.container, code, dockerMessage(body))
	}
}

func (d *Docker) Stop(ctx context.Context, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	q := url.Values{"t": {fmt.Sprint(seconds)}}
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+d.container+"/stop", q)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("docker: container %q does not exist", d.container)
	default:
		return fmt.Errorf("docker: stopping %q returned %d: %s", d.container, code, dockerMessage(body))
	}
}

// RecentLogs returns the tail of the container's log, including logs from
// previous runs of a stopped container — which is what lets napio learn the
// Factorio version of a server that is currently asleep.
func (d *Docker) RecentLogs(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	q := url.Values{
		"stdout": {"1"},
		"stderr": {"1"},
		"tail":   {fmt.Sprint(lines)},
	}
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+d.container+"/logs", q)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("docker: logs returned %d: %s", code, dockerMessage(body))
	}
	return demuxDockerLog(body), nil
}

// demuxDockerLog strips Docker's stream framing. Containers started without a
// TTY get an 8-byte header per frame; those with one are raw.
func demuxDockerLog(body []byte) string {
	var out strings.Builder
	for len(body) >= 8 {
		if body[0] > 2 || body[1] != 0 || body[2] != 0 || body[3] != 0 {
			return string(body) // not framed: a TTY stream
		}
		size := int(binary.BigEndian.Uint32(body[4:8]))
		if size < 0 || size > len(body)-8 {
			break
		}
		out.Write(body[8 : 8+size])
		body = body[8+size:]
	}
	if out.Len() == 0 {
		return string(body)
	}
	return out.String()
}

// CheckConfiguration reports deployment mistakes that would fight napio for
// control of the container. It is advisory: a warning, never a hard failure.
func (d *Docker) CheckConfiguration(ctx context.Context) []string {
	info, err := d.inspect(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	var warnings []string
	switch info.HostConfig.RestartPolicy.Name {
	case "always", "unless-stopped":
		warnings = append(warnings, fmt.Sprintf(
			"container %q has restart policy %q, which will restart Factorio behind napio's back; use \"no\"",
			d.container, info.HostConfig.RestartPolicy.Name))
	}
	return warnings
}

func dockerMessage(body []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Message != "" {
		return out.Message
	}
	return strings.TrimSpace(string(body))
}
