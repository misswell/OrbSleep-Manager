// Package runtime abstracts container engines behind one interface so the
// business engine never talks to a specific socket or engine type. The only
// implementation shipped today is dockerapi (every Docker-API-compatible
// engine: OrbStack, Docker Desktop, Colima, Rancher Desktop dockerd mode,
// Finch, Lima, Podman's compatibility socket, plain docker). Engines that do
// not implement the Docker API (containerd, CRI-O, Kubernetes) must NOT be
// shoved into this interface — they get their own adapters later, and until
// then they are simply not claimed as supported.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"

	"orbsleep/internal/dockerx"
)

// Kind identifies the engine family behind an environment.
type Kind string

const (
	KindOrbStack       Kind = "orbstack"
	KindDockerDesktop  Kind = "docker-desktop"
	KindDockerEngine   Kind = "docker-engine"
	KindColima         Kind = "colima"
	KindRancherDesktop Kind = "rancher-desktop"
	KindFinch          Kind = "finch"
	KindLima           Kind = "lima"
	KindPodman         Kind = "podman"
	KindCustom         Kind = "custom"
	// KindRemote is a Docker endpoint whose published ports do not land on
	// this Mac (SSH contexts, tcp:// daemons on other hosts). Port takeover
	// is not supported for it and must be reported honestly.
	KindRemote Kind = "remote"
)

// KnownKinds is the accepted set for user input validation.
var KnownKinds = []Kind{KindOrbStack, KindDockerDesktop, KindDockerEngine, KindColima,
	KindRancherDesktop, KindFinch, KindLima, KindPodman, KindCustom, KindRemote}

// ValidKind reports whether k is a known kind.
func ValidKind(k string) bool {
	for _, known := range KnownKinds {
		if string(known) == k {
			return true
		}
	}
	return false
}

// Capabilities declares what an environment can do. Every consumer must check
// before acting; a missing capability is reported to the user, never faked.
type Capabilities struct {
	ListContainers   bool `json:"listContainers"`
	InspectContainer bool `json:"inspectContainer"`
	StartStop        bool `json:"startStop"`
	Recreate         bool `json:"recreate"`       // create/remove/rename via the engine API
	Events           bool `json:"events"`         // push events (else reconcile polling only)
	PortTakeover     bool `json:"portTakeover"`   // published ports land on this Mac
	ComposeSupport   bool `json:"composeSupport"` // docker compose CLI usable against it
}

// Info describes one registered environment.
type Info struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Kind          Kind         `json:"kind"`
	Endpoint      string       `json:"endpoint"` // unix:///path or tcp://host:port
	EngineVersion string       `json:"engineVersion,omitempty"`
	APIVersion    string       `json:"apiVersion,omitempty"`
	Remote        bool         `json:"remote"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Runtime is the unified container engine interface.
type Runtime interface {
	Info() Info
	Ping(ctx context.Context) error
	ListContainers(ctx context.Context) ([]dockerx.ContainerSummary, error)
	InspectContainer(ctx context.Context, id string) (*dockerx.ContainerInspect, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSecs int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	CreateContainer(ctx context.Context, spec *ContainerSpec) (string, error)
	ConnectNetwork(ctx context.Context, network, containerID string, endpoint *EndpointConfig) error
	Events(ctx context.Context) (io.ReadCloser, error)
}

// EndpointConfig is the per-network endpoint payload for ConnectNetwork.
type EndpointConfig struct {
	Aliases    []string            `json:"Aliases,omitempty"`
	IPAMConfig *dockerx.IPAMConfig `json:"IPAMConfig,omitempty"`
}

// NormalizeEndpoint trims whitespace and guarantees the unix:// or tcp:// prefix.
func NormalizeEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	if strings.HasPrefix(ep, "unix://") || strings.HasPrefix(ep, "tcp://") {
		return ep
	}
	if strings.HasPrefix(ep, "/") {
		return "unix://" + ep
	}
	return ep
}

// EnvID derives the stable environment id from an endpoint. The id is
// content-addressed so the same socket discovered twice merges into one row.
func EnvID(endpoint string) string {
	norm := NormalizeEndpoint(endpoint)
	sum := sha256.Sum256([]byte(norm))
	return "env-" + hex.EncodeToString(sum[:])[:12]
}

// DisplayNameForKind gives discovered environments a friendly default name.
func DisplayNameForKind(kind Kind) string {
	switch kind {
	case KindOrbStack:
		return "OrbStack"
	case KindDockerDesktop:
		return "Docker Desktop"
	case KindColima:
		return "Colima"
	case KindRancherDesktop:
		return "Rancher Desktop"
	case KindFinch:
		return "Finch"
	case KindLima:
		return "Lima"
	case KindPodman:
		return "Podman"
	case KindDockerEngine:
		return "Docker Engine"
	case KindRemote:
		return "远程 Docker"
	default:
		return "自定义 Socket"
	}
}
