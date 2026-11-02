// Package docrun implements the "created without Compose" deployment strategy:
// a full inspect snapshot is mapped into an explicit recreation spec, the
// original container is kept as a rename-based rollback anchor until the
// replacement is verified, and restoring puts the original spec container
// back under its original name. Nothing is removed before its replacement is
// proven working, and anonymous volumes are re-attached by their original
// volume names so data is never dropped.
package docrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// Management labels added to the replacement container. They keep a managed
// container self-describing so recovery and restore can recognise it after a
// crash, and they carry the fingerprint of the managed configuration.
const (
	LabelManaged     = "orbsleep.managed"
	LabelServiceID   = "orbsleep.service-id"
	LabelStrategy    = "orbsleep.strategy"
	LabelFingerprint = "orbsleep.fingerprint"
	// BackupSuffixFormat is appended to the original container name during
	// attach ("<name>.orbsleep-bak-<origID12>"). The suffix is deterministic
	// so crash recovery can find the anchor without extra state.
	BackupSuffixFormat = "%s.orbsleep-bak-%s"
)

// BackupName returns the deterministic rollback-anchor name for a container.
func BackupName(name, originalID string) string {
	short := originalID
	if len(short) > 12 {
		short = short[:12]
	}
	short = strings.TrimPrefix(short, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf(BackupSuffixFormat, name, short)
}

// SpecFromInspect maps a full inspect payload into a lossless recreation spec.
// The mapping is explicit field by field — the inspect JSON is never submitted
// as a create request (runtime-resolved state must not leak into it).
func SpecFromInspect(ins *dockerx.ContainerInspect) (*runtime.ContainerSpec, error) {
	if ins == nil {
		return nil, fmt.Errorf("inspect 数据为空")
	}
	if strings.TrimSpace(ins.Config.Image) == "" && strings.TrimSpace(ins.Image) == "" {
		return nil, fmt.Errorf("容器缺少镜像信息，无法重建")
	}
	name := strings.TrimPrefix(ins.Name, "/")
	if name == "" {
		return nil, fmt.Errorf("容器缺少名称")
	}
	spec := &runtime.ContainerSpec{
		Name: name,
		Config: runtime.ConfigSpec{
			User:         ins.Config.User,
			Env:          copyStrings(ins.Config.Env),
			Cmd:          copyStrings(ins.Config.Cmd),
			Image:        firstNonEmpty(ins.Config.Image, ins.Image),
			Volumes:      copyVolumeSet(ins.Config.Volumes),
			WorkingDir:   ins.Config.WorkingDir,
			Entrypoint:   copyStrings(ins.Config.Entrypoint),
			Labels:       copyLabels(ins.Config.Labels),
			StopSignal:   ins.Config.StopSignal,
			StopTimeout:  ins.Config.StopTimeout,
			Healthcheck:  copyHealthcheck(ins.Config.Healthcheck),
			Tty:          ins.Config.Tty,
			OpenStdin:    ins.Config.OpenStdin,
			StdinOnce:    ins.Config.StdinOnce,
			ExposedPorts: copyExposed(ins.Config.ExposedPorts),
		},
		HostConfig: runtime.HostConfigSpec{
			Binds:                copyStrings(ins.HostConfig.Binds),
			NetworkMode:          defaultString(ins.HostConfig.NetworkMode, "default"),
			PortBindings:         copyPortBindings(ins.HostConfig.PortBindings),
			RestartPolicy:        dockerx.RestartPolicy{Name: ins.HostConfig.RestartPolicy.Name, MaximumRetryCount: ins.HostConfig.RestartPolicy.MaximumRetryCount},
			AutoRemove:           ins.HostConfig.AutoRemove,
			CapAdd:               copyStrings(ins.HostConfig.CapAdd),
			CapDrop:              copyStrings(ins.HostConfig.CapDrop),
			CgroupnsMode:         ins.HostConfig.CgroupnsMode,
			Dns:                  copyStrings(ins.HostConfig.Dns),
			DnsOptions:           copyStrings(ins.HostConfig.DnsOptions),
			DnsSearch:            copyStrings(ins.HostConfig.DnsSearch),
			ExtraHosts:           copyStrings(ins.HostConfig.ExtraHosts),
			GroupAdd:             copyStrings(ins.HostConfig.GroupAdd),
			IpcMode:              ins.HostConfig.IpcMode,
			Cgroup:               ins.HostConfig.Cgroup,
			Links:                copyStrings(ins.HostConfig.Links),
			OomScoreAdj:          ins.HostConfig.OomScoreAdj,
			PidMode:              ins.HostConfig.PidMode,
			Privileged:           ins.HostConfig.Privileged,
			PublishAllPorts:      ins.HostConfig.PublishAllPorts,
			ReadonlyRootfs:       ins.HostConfig.ReadonlyRootfs,
			SecurityOpt:          copyStrings(ins.HostConfig.SecurityOpt),
			StorageOpt:           copyStringMap(ins.HostConfig.StorageOpt),
			Tmpfs:                copyStringMap(ins.HostConfig.Tmpfs),
			UTSMode:              ins.HostConfig.UTSMode,
			UsernsMode:           ins.HostConfig.UsernsMode,
			ShmSize:              ins.HostConfig.ShmSize,
			Sysctls:              copyStringMap(ins.HostConfig.Sysctls),
			Runtime:              ins.HostConfig.Runtime,
			Isolation:            ins.HostConfig.Isolation,
			Devices:              copyDevices(ins.HostConfig.Devices),
			DeviceCgroupRules:    copyStrings(ins.HostConfig.DeviceCgroupRules),
			DeviceRequests:       copyDeviceRequests(ins.HostConfig.DeviceRequests),
			Init:                 ins.HostConfig.Init,
			Mounts:               copyMounts(ins.HostConfig.Mounts),
			LogConfig:            copyLogConfig(ins.HostConfig.LogConfig),
			Memory:               ins.HostConfig.Memory,
			MemorySwap:           ins.HostConfig.MemorySwap,
			MemorySwappiness:     ins.HostConfig.MemorySwappiness,
			MemoryReservation:    ins.HostConfig.MemoryReservation,
			KernelMemoryTCP:      ins.HostConfig.KernelMemoryTCP,
			OomKillDisable:       ins.HostConfig.OomKillDisable,
			CPUShares:            ins.HostConfig.CPUShares,
			CPUQuota:             ins.HostConfig.CPUQuota,
			CPUPeriod:            ins.HostConfig.CPUPeriod,
			CPURealtimePeriod:    ins.HostConfig.CPURealtimePeriod,
			CPURealtimeRuntime:   ins.HostConfig.CPURealtimeRuntime,
			NanoCpus:             ins.HostConfig.NanoCpus,
			CpusetCpus:           ins.HostConfig.CpusetCpus,
			CpusetMems:           ins.HostConfig.CpusetMems,
			CPUCount:             ins.HostConfig.CPUCount,
			CPUPercent:           ins.HostConfig.CPUPercent,
			PidsLimit:            ins.HostConfig.PidsLimit,
			BlkioWeight:          ins.HostConfig.BlkioWeight,
			BlkioWeightDevice:    dockerx.CopyWeightDevices(ins.HostConfig.BlkioWeightDevice),
			BlkioDeviceReadBps:   dockerx.CopyThrottleDevices(ins.HostConfig.BlkioDeviceReadBps),
			BlkioDeviceWriteBps:  dockerx.CopyThrottleDevices(ins.HostConfig.BlkioDeviceWriteBps),
			BlkioDeviceReadIOps:  dockerx.CopyThrottleDevices(ins.HostConfig.BlkioDeviceReadIOps),
			BlkioDeviceWriteIOps: dockerx.CopyThrottleDevices(ins.HostConfig.BlkioDeviceWriteIOps),
			Ulimits:              copyUlimits(ins.HostConfig.Ulimits),
		},
	}

	// Hostname: Docker auto-fills it with the container's short ID when the
	// user did not set one. Pinning that value on the replacement would chain
	// it to the old container identity, so only keep a genuinely user-set
	// hostname.
	if h := strings.TrimSpace(ins.Config.Hostname); h != "" && !isShortID(h) {
		spec.Config.Hostname = h
	}
	if d := strings.TrimSpace(ins.Config.Domainname); d != "" {
		spec.Config.Domainname = d
	}

	// ExposedPorts must stay consistent with PortBindings: every published
	// container port needs an entry, otherwise some engines drop the binding.
	if spec.Config.ExposedPorts == nil {
		spec.Config.ExposedPorts = map[string]struct{}{}
	}
	for key := range ins.HostConfig.PortBindings {
		if _, ok := spec.Config.ExposedPorts[key]; !ok {
			spec.Config.ExposedPorts[key] = struct{}{}
		}
	}
	if len(spec.Config.ExposedPorts) == 0 {
		spec.Config.ExposedPorts = nil
	}

	// Anonymous volumes: the image-declared or CLI-created volumes exist only
	// as resolved Mounts entries. Re-attach each by its original volume name
	// so the replacement keeps the data instead of getting a fresh empty
	// volume. Named volumes and binds are already in Binds/Mounts and are
	// skipped by the destination check.
	for _, m := range ins.Mounts {
		if m.Type != "volume" || !dockerx.IsAnonymousVolume(m.Name) {
			continue
		}
		if destCoveredBy(m.Destination, ins.HostConfig.Binds, ins.HostConfig.Mounts) {
			continue
		}
		bind := m.Name + ":" + m.Destination
		if !m.RW {
			bind += ":ro"
		}
		spec.HostConfig.Binds = append(spec.HostConfig.Binds, bind)
	}

	// Networking: primary endpoint via NetworkingConfig, secondary networks
	// reconnected after create. Static IPAM config is preserved; if the old
	// container still holds the IP at create time the create fails and rolls
	// back rather than silently changing the address.
	primary := PrimaryNetwork(ins)
	spec.Networks = map[string]runtime.EndpointConfig{}
	for netName, ep := range ins.NetworkSettings.Networks {
		if netName != primary {
			continue
		}
		ec := runtime.EndpointConfig{}
		if aliases := userAliases(ep, ins); len(aliases) > 0 {
			// Set both spellings: Docker 29.x only honors DNSNames on raw
			// API creates, older engines only know Aliases.
			ec.Aliases = aliases
			ec.DNSNames = copyStrings(aliases)
		}
		if ep.IPAMConfig != nil {
			ec.IPAMConfig = &dockerx.IPAMConfig{
				IPv4Address:  ep.IPAMConfig.IPv4Address,
				IPv6Address:  ep.IPAMConfig.IPv6Address,
				LinkLocalIPs: copyStrings(ep.IPAMConfig.LinkLocalIPs),
			}
		}
		spec.Networks[netName] = ec
	}
	return spec, nil
}

// PrimaryNetwork resolves which network is the container's primary (the one
// NetworkMode refers to). "default"/empty means the bridge network.
func PrimaryNetwork(ins *dockerx.ContainerInspect) string {
	mode := ins.HostConfig.NetworkMode
	switch mode {
	case "", "default":
		return "bridge"
	}
	if strings.HasPrefix(mode, "container:") ||
		strings.HasPrefix(mode, "host") || mode == "none" {
		// Host/none containers have no endpoint config to preserve; callers
		// block these before attach. Fall back to the first observed network.
		for name := range ins.NetworkSettings.Networks {
			return name
		}
		return mode
	}
	return mode
}

// SecondaryNetworks lists the networks that must be re-connected after create.
func SecondaryNetworks(ins *dockerx.ContainerInspect) []string {
	primary := PrimaryNetwork(ins)
	var out []string
	for name, ep := range ins.NetworkSettings.Networks {
		if name == primary {
			continue
		}
		if strings.HasPrefix(name, "orbsleep") {
			// Never re-attach to networks that only existed for OrbSleep's own
			// wiring (defensive; none are created today).
			continue
		}
		_ = ep
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Fingerprint is the canonical identity of the recreation-relevant config.
// It deliberately excludes management labels so comparing a managed container
// against its snapshot ignores OrbSleep's own metadata.
func Fingerprint(spec *runtime.ContainerSpec) string {
	clone := *spec
	clone.Config.Labels = map[string]string{}
	for k, v := range spec.Config.Labels {
		if strings.HasPrefix(k, "orbsleep.") {
			continue
		}
		clone.Config.Labels[k] = v
	}
	// Name is part of the identity but the backup anchor has a different name;
	// normalize by clearing it (callers compare specs of the same logical
	// container under possibly different names).
	clone.Name = ""
	raw, err := json.Marshal(&clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// RemoveTCPPortBindings drops the given takeover ports from a spec. UDP and
// non-takeover ports are preserved untouched.
func RemoveTCPPortBindings(spec *runtime.ContainerSpec, removals map[string]struct{}) {
	out := map[string][]dockerx.PortBinding{}
	for key, bindings := range spec.HostConfig.PortBindings {
		containerPort, proto := splitPortKey(key)
		if proto == "tcp" {
			if _, drop := removals[key]; drop {
				continue
			}
			_ = containerPort
		}
		out[key] = bindings
	}
	spec.HostConfig.PortBindings = out
	// ExposedPorts can stay: exposing a port without publishing it is legal
	// and preserves inter-container reachability semantics.
}

func splitPortKey(key string) (int, string) {
	proto := "tcp"
	portStr := key
	if i := strings.Index(key, "/"); i >= 0 {
		portStr, proto = key[:i], key[i+1:]
	}
	n := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return 0, proto
		}
		n = n*10 + int(r-'0')
	}
	return n, proto
}

// userAliases merges the legacy Aliases field with the modern DNSNames field
// and strips the container's own identities (name, short id) — those are
// implicit on every container and pinning them would chain the replacement
// to the original's identity.
func userAliases(ep dockerx.EndpointSettings, ins *dockerx.ContainerInspect) []string {
	name := strings.TrimPrefix(ins.Name, "/")
	own := map[string]bool{name: true, ins.ID: true}
	if len(ins.ID) >= 12 {
		own[ins.ID[:12]] = true
	}
	seen := map[string]bool{}
	var out []string
	add := func(list []string) {
		for _, a := range list {
			a = strings.TrimPrefix(a, "/")
			if a == "" || own[a] || seen[a] {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	add(ep.Aliases)
	add(ep.DNSNames)
	sort.Strings(out)
	return out
}

func destCoveredBy(dest string, binds []string, mounts []dockerx.MountSpec) bool {
	for _, b := range binds {
		parts := strings.SplitN(b, ":", 3)
		target := dest
		switch len(parts) {
		case 1:
			target = parts[0] // anonymous-style "dest"
		case 2:
			target = parts[1]
		default:
			target = parts[1]
			if strings.Contains(b, ":ro") || strings.Contains(b, ":rw") {
				// name:dest:mode
				target = parts[1]
			}
		}
		if target == dest {
			return true
		}
	}
	for _, m := range mounts {
		if m.Target == dest {
			return true
		}
	}
	return false
}

func isShortID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyVolumeSet(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func copyExposed(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func copyPortBindings(in map[string][]dockerx.PortBinding) map[string][]dockerx.PortBinding {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]dockerx.PortBinding, len(in))
	for k, v := range in {
		list := make([]dockerx.PortBinding, len(v))
		copy(list, v)
		out[k] = list
	}
	return out
}

func copyHealthcheck(in *dockerx.Healthcheck) *dockerx.Healthcheck {
	if in == nil {
		return nil
	}
	out := *in
	out.Test = copyStrings(in.Test)
	return &out
}

func copyDevices(in []dockerx.DeviceMapping) []dockerx.DeviceMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]dockerx.DeviceMapping, len(in))
	copy(out, in)
	return out
}

func copyDeviceRequests(in []dockerx.DeviceRequest) []dockerx.DeviceRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]dockerx.DeviceRequest, len(in))
	for i, d := range in {
		out[i] = d
		out[i].DeviceIDs = copyStrings(d.DeviceIDs)
		out[i].Options = copyStringMap(d.Options)
		if d.Capabilities != nil {
			out[i].Capabilities = make([][]string, len(d.Capabilities))
			for j, caps := range d.Capabilities {
				out[i].Capabilities[j] = copyStrings(caps)
			}
		}
	}
	return out
}

func copyMounts(in []dockerx.MountSpec) []dockerx.MountSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]dockerx.MountSpec, len(in))
	for i, m := range in {
		out[i] = m
		if m.BindOptions != nil {
			bo := *m.BindOptions
			out[i].BindOptions = &bo
		}
		if m.VolumeOptions != nil {
			vo := *m.VolumeOptions
			vo.Labels = copyStringMap(m.VolumeOptions.Labels)
			vo.DriverConfig = copyStringMap(m.VolumeOptions.DriverConfig)
			out[i].VolumeOptions = &vo
		}
		if m.TmpfsOptions != nil {
			to := *m.TmpfsOptions
			out[i].TmpfsOptions = &to
		}
	}
	return out
}

func copyLogConfig(in dockerx.LogConfig) *dockerx.LogConfig {
	if in.Type == "" && len(in.Config) == 0 {
		return nil
	}
	return &dockerx.LogConfig{Type: in.Type, Config: copyStringMap(in.Config)}
}

func copyUlimits(in []dockerx.Ulimit) []dockerx.Ulimit {
	if len(in) == 0 {
		return nil
	}
	out := make([]dockerx.Ulimit, len(in))
	copy(out, in)
	return out
}
