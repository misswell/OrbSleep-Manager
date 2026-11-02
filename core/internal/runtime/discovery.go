package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orbsleep/internal/dockerx"
)

// Candidate is one discoverable container environment endpoint.
type Candidate struct {
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Endpoint string `json:"endpoint"` // unix:///... or tcp://...
	Source   string `json:"source"`   // env | context | orbstack | docker-desktop | colima | ...
	Remote   bool   `json:"remote"`
}

// Discover enumerates every plausible local container endpoint without
// contacting any of them. Deterministic order: env overrides, well-known
// runtime sockets, then Docker contexts. Duplicates are merged by endpoint.
func Discover() []Candidate {
	home, _ := os.UserHomeDir()
	var out []Candidate
	seen := map[string]bool{}

	add := func(name string, kind Kind, endpoint, source string) {
		ep := NormalizeEndpoint(endpoint)
		if ep == "" {
			return
		}
		remote := strings.HasPrefix(ep, "tcp://")
		if remote {
			kind = KindRemote
		}
		if seen[ep] {
			return
		}
		seen[ep] = true
		out = append(out, Candidate{Name: name, Kind: kind, Endpoint: ep, Source: source, Remote: remote})
	}

	if v := strings.TrimSpace(os.Getenv("ORBSLEEP_DOCKER_SOCKET")); v != "" {
		add("ORBSLEEP_DOCKER_SOCKET", KindFromPath(v), v, "env")
	}
	if v := strings.TrimSpace(os.Getenv("DOCKER_HOST")); v != "" {
		add("DOCKER_HOST", KindFromPath(v), v, "env")
	}

	// Well-known per-runtime socket locations. Every hit is a candidate even
	// when the daemon is down: the environment page shows reachability.
	add("OrbStack", KindOrbStack, filepath.Join(home, ".orbstack", "run", "docker.sock"), "orbstack")
	add("Docker Desktop", KindDockerDesktop, filepath.Join(home, ".docker", "run", "docker.sock"), "docker-desktop")
	add("Docker Desktop (系统)", KindDockerDesktop, "/var/run/docker.sock", "docker-desktop")
	add("系统 Docker", KindDockerEngine, "/var/run/docker.sock", "default")

	// Colima / Lima profiles.
	for _, pattern := range []string{
		filepath.Join(home, ".colima", "*", "docker.sock"),
		filepath.Join(home, ".colima", "docker.sock"),
	} {
		for _, p := range globSorted(pattern) {
			name := "Colima"
			if base := filepath.Base(filepath.Dir(p)); base != "" && base != "docker.sock" && base != "." {
				name = "Colima (" + base + ")"
			}
			add(name, KindColima, p, "colima")
		}
	}
	for _, p := range globSorted(filepath.Join(home, ".lima", "*", "sock", "docker.sock")) {
		name := "Lima"
		if base := filepath.Base(filepath.Dir(filepath.Dir(p))); base != "" && base != "sock" {
			name = "Lima (" + base + ")"
		}
		add(name, KindLima, p, "lima")
	}

	// Rancher Desktop (dockerd mode) and Finch.
	add("Rancher Desktop", KindRancherDesktop, filepath.Join(home, ".rd", "docker.sock"), "rancher-desktop")
	add("Finch", KindFinch, filepath.Join(home, ".finch", "docker.sock"), "finch")
	for _, p := range globSorted(filepath.Join(home, ".finch", "*", "docker.sock")) {
		add("Finch", KindFinch, p, "finch")
	}

	// Podman machine / rootless.
	add("Podman", KindPodman, filepath.Join(home, ".local", "share", "containers", "podman.sock"), "podman")
	add("Podman (系统)", KindPodman, "/var/run/podman/podman.sock", "podman")

	// Docker contexts (includes user-created ones; ssh:// and tcp:// endpoints
	// surface as remote environments).
	metaDir := filepath.Join(home, ".docker", "contexts", "meta")
	if entries, err := os.ReadDir(metaDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(metaDir, entry.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var meta struct {
				Name      string `json:"Name"`
				Endpoints map[string]struct {
					Host string `json:"Host"`
				} `json:"Endpoints"`
			}
			if json.Unmarshal(raw, &meta) != nil || meta.Name == "" {
				continue
			}
			if host := meta.Endpoints["docker"].Host; host != "" {
				add(meta.Name, KindFromPath(host), host, "context")
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return sourceOrder(out[i].Source) < sourceOrder(out[j].Source)
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out
}

func sourceOrder(s string) int {
	switch s {
	case "env":
		return 0
	case "orbstack":
		return 1
	case "docker-desktop":
		return 2
	case "colima", "lima", "rancher-desktop", "finch", "podman":
		return 3
	case "context":
		return 4
	default:
		return 5
	}
}

func globSorted(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// KindFromPath classifies a socket path without contacting the engine. It
// delegates to dockerx's authoritative classifier and maps its string kinds.
func KindFromPath(pathOrEndpoint string) Kind {
	switch dockerx.KindFromPath(strings.TrimPrefix(strings.TrimSpace(pathOrEndpoint), "unix://")) {
	case "orbstack":
		return KindOrbStack
	case "colima":
		return KindColima
	case "lima":
		return KindLima
	case "rancher-desktop":
		return KindRancherDesktop
	case "finch":
		return KindFinch
	case "podman":
		return KindPodman
	case "docker-desktop":
		return KindDockerDesktop
	default:
		return KindDockerEngine
	}
}

// ProbeResult is the reachability verdict for one candidate endpoint.
type ProbeResult struct {
	Reachable     bool
	Kind          Kind
	EngineVersion string
	APIVersion    string
	Error         string
}

// Probe connects to an endpoint and identifies the engine family from its own
// /version payload. Podman's compatibility socket reports itself there, so a
// socket path alone never decides what runs behind it.
func Probe(ctx context.Context, endpoint string) ProbeResult {
	norm := NormalizeEndpoint(endpoint)
	if !strings.HasPrefix(norm, "unix://") {
		return ProbeResult{Kind: KindRemote, Error: "当前仅支持 unix socket 端点"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	client, err := dockerx.New(probeCtx, strings.TrimPrefix(norm, "unix://"))
	if err != nil {
		return ProbeResult{Kind: KindFromPath(norm), Error: err.Error()}
	}
	v, err := client.FullVersion(probeCtx)
	if err != nil {
		return ProbeResult{Kind: KindFromPath(norm), Error: err.Error()}
	}
	res := ProbeResult{Reachable: true, Kind: KindFromPath(norm), EngineVersion: v.Version, APIVersion: v.APIVersion}
	for _, comp := range v.Components {
		name := strings.ToLower(comp.Name)
		if strings.Contains(name, "podman") {
			res.Kind = KindPodman
			break
		}
	}
	return res
}

// ValidateCustomEndpoint checks a user-supplied endpoint before it is stored.
func ValidateCustomEndpoint(ctx context.Context, endpoint string) (ProbeResult, error) {
	norm := NormalizeEndpoint(endpoint)
	if norm == "" {
		return ProbeResult{}, fmt.Errorf("端点不能为空")
	}
	res := Probe(ctx, norm)
	if !res.Reachable {
		return res, fmt.Errorf("端点不可达: %s", res.Error)
	}
	return res, nil
}
