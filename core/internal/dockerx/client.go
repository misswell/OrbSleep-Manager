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

func contextKind(socket string) string {
	if strings.Contains(socket, ".orbstack") {
		return "orbstack"
	}
	return "docker"
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
