// Package dockerapi implements the runtime.Runtime interface against any
// Docker Engine API compatible endpoint (unix socket or tcp). It is the single
// adapter for OrbStack, Docker Desktop, Colima, Rancher Desktop (dockerd
// mode), Finch, Lima, plain Docker Engine and Podman's compatibility socket.
package dockerapi

import (
	"context"
	"fmt"
	"io"
	"strings"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// Runtime is a Docker-API-compatible engine bound to one endpoint.
type Runtime struct {
	client *dockerx.Client
	info   runtime.Info
}

// New connects to the endpoint (unix:///path or tcp://host:port) and pings it.
func New(ctx context.Context, name string, kind runtime.Kind, endpoint string) (*Runtime, error) {
	norm := runtime.NormalizeEndpoint(endpoint)
	if !strings.HasPrefix(norm, "unix://") {
		return nil, fmt.Errorf("当前仅支持 unix socket 端点: %s", norm)
	}
	socket := strings.TrimPrefix(norm, "unix://")
	client, err := dockerx.New(ctx, socket)
	if err != nil {
		return nil, err
	}
	return newWithClient(client, name, kind), nil
}

// NewWithClient wraps an existing dockerx client (used by the registry cache).
func NewWithClient(client *dockerx.Client, name string, kind runtime.Kind) *Runtime {
	return newWithClient(client, name, kind)
}

func newWithClient(client *dockerx.Client, name string, kind runtime.Kind) *Runtime {
	socket := client.Socket()
	caps := runtime.Capabilities{
		ListContainers:   true,
		InspectContainer: true,
		StartStop:        true,
		Recreate:         true,
		Events:           true,
		PortTakeover:     true, // local VM runtimes publish ports on this Mac
		ComposeSupport:   true,
	}
	info := runtime.Info{
		Name:         name,
		Kind:         kind,
		Endpoint:     "unix://" + socket,
		Remote:       false,
		Capabilities: caps,
	}
	if kind == runtime.KindRemote || kind == runtime.KindCustom {
		// Only a tcp:// endpoint is definitively remote; a custom unix socket
		// is still assumed local until proven otherwise.
		if strings.HasPrefix(info.Endpoint, "tcp://") {
			info.Remote = true
			info.Capabilities.PortTakeover = false
		}
	}
	return &Runtime{client: client, info: info}
}

// Info returns the environment description.
func (r *Runtime) Info() runtime.Info { return r.info }

// RefreshInfo updates the cached engine version labels from /version.
func (r *Runtime) RefreshInfo(ctx context.Context) {
	if v, err := r.client.FullVersion(ctx); err == nil {
		r.info.EngineVersion = v.Version
		r.info.APIVersion = v.APIVersion
	}
}

// Client exposes the underlying dockerx client for paths that still need raw
// access (health checks, proxy IP lookup). New code should prefer the Runtime
// interface.
func (r *Runtime) Client() *dockerx.Client { return r.client }

// Socket returns the unix socket path.
func (r *Runtime) Socket() string { return r.client.Socket() }

// Ping verifies the engine answers.
func (r *Runtime) Ping(ctx context.Context) error { return r.client.Get(ctx, "/_ping", nil) }

// ListContainers lists all containers (running or not).
func (r *Runtime) ListContainers(ctx context.Context) ([]dockerx.ContainerSummary, error) {
	return r.client.ListContainers(ctx)
}

// InspectContainer returns the full inspect payload.
func (r *Runtime) InspectContainer(ctx context.Context, id string) (*dockerx.ContainerInspect, error) {
	return r.client.InspectContainer(ctx, id)
}

// StartContainer starts a container.
func (r *Runtime) StartContainer(ctx context.Context, id string) error {
	return r.client.StartContainer(ctx, id)
}

// StopContainer stops a container with the given grace period.
func (r *Runtime) StopContainer(ctx context.Context, id string, timeoutSecs int) error {
	return r.client.StopContainer(ctx, id, timeoutSecs)
}

// RemoveContainer removes a container.
func (r *Runtime) RemoveContainer(ctx context.Context, id string, force bool) error {
	return r.client.RemoveContainer(ctx, id, force)
}

// RenameContainer renames a container.
func (r *Runtime) RenameContainer(ctx context.Context, id, name string) error {
	return r.client.RenameContainer(ctx, id, name)
}

// CreateContainer builds the Engine API create request from the spec and
// returns the new container id.
func (r *Runtime) CreateContainer(ctx context.Context, spec *runtime.ContainerSpec) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("容器规格为空")
	}
	return r.client.CreateContainer(ctx, spec.Name, runtime.CreateRequestBody(spec))
}

// ConnectNetwork attaches a container to one additional network.
func (r *Runtime) ConnectNetwork(ctx context.Context, network, containerID string, endpoint *runtime.EndpointConfig) error {
	return r.client.ConnectNetwork(ctx, network, containerID, endpoint)
}

// Events streams container events until the context is cancelled.
func (r *Runtime) Events(ctx context.Context) (io.ReadCloser, error) {
	return r.client.Events(ctx)
}

// Capabilities returns the declared capability set.
func (r *Runtime) Capabilities() runtime.Capabilities { return r.info.Capabilities }
