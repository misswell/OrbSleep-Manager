// Package dockerx talks to the Docker Engine API over the local Unix socket.
// It never changes the user's global Docker context: callers pass the socket
// discovered by Detect to all other tools (including the docker CLI).
package dockerx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbsleep/internal/model"
)

// Client is a Docker Engine API client bound to one Unix socket.
type Client struct {
	socket  string
	http    *http.Client
	context model.ContextInfo
	version string
	apiVer  string
}

// Version is the /version payload subset we care about.
type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
}

// APIError is a non-2xx Docker API response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker API %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// New creates a client for an explicit socket path. It pings the daemon.
func New(ctx context.Context, socket string) (*Client, error) {
	c := &Client{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "unix", socket)
			},
			DisableCompression: true,
			MaxIdleConns:       16,
		}},
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var v Version
	if err := c.get(pingCtx, "/version", &v); err != nil {
		return nil, err
	}
	c.version, c.apiVer = v.Version, v.APIVersion
	c.context = model.ContextInfo{Name: detectContextName(socket), Endpoint: "unix://" + socket, Kind: contextKind(socket)}
	return c, nil
}

// Detect locates a usable Docker socket, preferring OrbStack.
func Detect(ctx context.Context) (*Client, error) {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("ORBSLEEP_DOCKER_SOCKET")); v != "" {
		candidates = append(candidates, strings.TrimPrefix(v, "unix://"))
	}
	if v := strings.TrimSpace(os.Getenv("DOCKER_HOST")); strings.HasPrefix(v, "unix://") {
		candidates = append(candidates, strings.TrimPrefix(v, "unix://"))
	}
	if ep := contextEndpoint("orbstack"); ep != "" {
		candidates = append(candidates, ep)
	}
	home, _ := os.UserHomeDir()
	candidates = append(candidates,
		filepath.Join(home, ".orbstack", "run", "docker.sock"),
		filepath.Join(home, ".docker", "run", "docker.sock"),
		"/var/run/docker.sock",
	)

	seen := map[string]bool{}
	var errs []string
	for _, socket := range candidates {
		if socket == "" || seen[socket] {
			continue
		}
		seen[socket] = true
		if _, err := os.Stat(socket); err != nil {
			continue
		}
		c, err := New(ctx, socket)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", socket, err))
			continue
		}
		return c, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("未找到可用的 Docker socket（OrbStack 可能未启动）")
	}
	return nil, fmt.Errorf("Docker socket 不可用: %s", strings.Join(errs, "; "))
}

// Socket returns the Unix socket path.
func (c *Client) Socket() string { return c.socket }

// Context returns the detected context information.
func (c *Client) Context() model.ContextInfo { return c.context }

// DockerHost is the DOCKER_HOST value matching this client.
func (c *Client) DockerHost() string { return "unix://" + c.socket }

// Version returns the daemon version string.
func (c *Client) Version() string { return c.version }

// APIVersion returns the Docker API version.
func (c *Client) APIVersion() string { return c.apiVer }

// Get performs a GET request and decodes JSON into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.get(ctx, path, out)
}

// Stream opens a streaming GET (used for /events).
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	// No client timeout: the caller cancels via ctx.
	streamC := &http.Client{Transport: c.http.Transport}
	resp, err := streamC.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: string(body)}
	}
	return resp.Body, nil
}

// Post performs a POST request with an optional JSON body.
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *c.http
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListContainers returns all containers, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	var out []ContainerSummary
	if err := c.Get(ctx, "/containers/json?all=1", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectContainer returns the full container inspect payload.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInspect, error) {
	var out ContainerInspect
	if err := c.Get(ctx, "/containers/"+id+"/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveContainer removes a container (force stops it first when requested).
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	path := "/containers/" + id
	if force {
		path += "?force=1"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// StartContainer starts a stopped container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

// StopContainer stops a running container, waiting up to timeout seconds.
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	path := "/containers/" + id + "/stop"
	if timeout > 0 {
		path += fmt.Sprintf("?t=%d", timeout)
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// RenameContainer renames a container. Used by the docker-run recreate path to
// keep the original container as an instant rollback anchor while the
// replacement is built under the original name.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/rename?name="+url.QueryEscape(name), nil, nil)
}

// CreateContainer creates a container and returns its ID. The body is the
// Engine API create request (Config/HostConfig/NetworkingConfig at top level).
func (c *Client) CreateContainer(ctx context.Context, name string, body any) (string, error) {
	path := "/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("容器创建响应缺少 Id")
	}
	return out.ID, nil
}

// ConnectNetwork attaches a container to a network. endpoint may be nil.
func (c *Client) ConnectNetwork(ctx context.Context, network, containerID string, endpoint any) error {
	body := struct {
		Container      string `json:"Container"`
		EndpointConfig any    `json:"EndpointConfig,omitempty"`
	}{Container: containerID, EndpointConfig: endpoint}
	return c.do(ctx, http.MethodPost, "/networks/"+url.QueryEscape(network)+"/connect", body, nil)
}

// DisconnectNetwork detaches a container from a network.
func (c *Client) DisconnectNetwork(ctx context.Context, network, containerID string) error {
	body := struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}{Container: containerID}
	return c.do(ctx, http.MethodPost, "/networks/"+url.QueryEscape(network)+"/disconnect", body, nil)
}

// FullVersion returns the engine identity payload (used to label environments).
func (c *Client) FullVersion(ctx context.Context) (*EngineVersion, error) {
	var v EngineVersion
	if err := c.get(ctx, "/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Events subscribes to Docker events. The caller must close the body.
func (c *Client) Events(ctx context.Context) (io.ReadCloser, error) {
	return c.Stream(ctx, "/events?filters="+urlQueryEscape(`{"type":["container"]}`))
}

func urlQueryEscape(s string) string {
	// Minimal percent-encoding for the JSON filter value.
	replacer := strings.NewReplacer(
		"%", "%25", " ", "%20", `"`, "%22", "{", "%7B", "}", "%7D",
		"[", "%5B", "]", "%5D", ":", "%3A", ",", "%2C",
	)
	return replacer.Replace(s)
}

func detectContextName(socket string) string {
	if strings.Contains(socket, ".orbstack") {
		return "orbstack"
	}
	if name := currentContext(); name != "" {
		if ep := contextEndpoint(name); ep != "" && ep == socket {
			return name
		}
	}
	return "default"
}

// contextKind labels a socket path with its runtime family. OrbStack keeps its
// historical kind value; other runtimes are recognised by their well-known
// socket locations. The runtime package refines the kind with the engine
// identity returned by /version.
func contextKind(socket string) string {
	switch KindFromPath(socket) {
	case "orbstack", "colima", "rancher-desktop", "finch", "lima", "podman", "docker-desktop":
		return KindFromPath(socket)
	}
	return "docker"
}

// KindFromPath classifies a socket path without contacting the engine.
func KindFromPath(socket string) string {
	home, _ := os.UserHomeDir()
	homePrefix := filepath.ToSlash(home)
	path := filepath.ToSlash(socket)
	switch {
	case strings.Contains(path, ".orbstack"):
		return "orbstack"
	case strings.Contains(path, ".colima"), strings.Contains(path, ".lima"):
		if strings.Contains(path, ".colima") {
			return "colima"
		}
		return "lima"
	case strings.Contains(path, "/.rd/"):
		return "rancher-desktop"
	case strings.Contains(path, ".finch"):
		return "finch"
	case strings.Contains(path, "podman"):
		return "podman"
	case strings.Contains(path, ".docker/run") || strings.Contains(path, ".docker/desktop"):
		return "docker-desktop"
	case path == "/var/run/docker.sock":
		return "docker"
	case homePrefix != "" && strings.HasPrefix(path, homePrefix):
		// Unknown socket under the user's home: treat as generic docker.
		return "docker"
	default:
		return "docker"
	}
}

func currentContext() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return cfg.CurrentContext
}

// contextEndpoint returns the docker endpoint of a named context, if present.
func contextEndpoint(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	metaPath := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var meta struct {
		Name      string `json:"Name"`
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	host := meta.Endpoints.Docker.Host
	if strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return ""
}

// IsErrNotFound reports whether err is a Docker 404.
func IsErrNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
