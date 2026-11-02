package runtime

import (
	"orbsleep/internal/dockerx"
)

// ContainerSpec is the full, lossless recreation contract for a container.
// It mirrors the Engine API create request (Config/HostConfig/NetworkingConfig)
// but only carries fields OrbSleep deliberately preserves; everything else is
// intentionally dropped. The docker-run deployment adapter builds a spec from
// a full inspect with explicit field mapping — the inspect JSON is never
// submitted as a create request directly.
type ContainerSpec struct {
	Name       string                    `json:"name"`
	Config     ConfigSpec                `json:"config"`
	HostConfig HostConfigSpec            `json:"hostConfig"`
	Networks   map[string]EndpointConfig `json:"networks,omitempty"`
}

// ConfigSpec is the container configuration section.
type ConfigSpec struct {
	Hostname     string               `json:"Hostname,omitempty"`
	Domainname   string               `json:"Domainname,omitempty"`
	User         string               `json:"User,omitempty"`
	Env          []string             `json:"Env,omitempty"`
	Cmd          []string             `json:"Cmd,omitempty"`
	Image        string               `json:"Image"`
	Volumes      map[string]struct{}  `json:"Volumes,omitempty"`
	WorkingDir   string               `json:"WorkingDir,omitempty"`
	Entrypoint   []string             `json:"Entrypoint,omitempty"`
	Labels       map[string]string    `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{}  `json:"ExposedPorts,omitempty"`
	StopSignal   string               `json:"StopSignal,omitempty"`
	StopTimeout  *int                 `json:"StopTimeout,omitempty"`
	Healthcheck  *dockerx.Healthcheck `json:"Healthcheck,omitempty"`
	Tty          bool                 `json:"Tty,omitempty"`
	OpenStdin    bool                 `json:"OpenStdin,omitempty"`
	StdinOnce    bool                 `json:"StdinOnce,omitempty"`
}

// HostConfigSpec is the host-side configuration section. Field for field it
// matches what the Engine API accepts on create; runtime-resolved state
// (container IDs, IPs, mount sources of auto-created anonymous volumes) is
// never copied blindly.
type HostConfigSpec struct {
	Binds             []string                         `json:"Binds,omitempty"`
	NetworkMode       string                           `json:"NetworkMode,omitempty"`
	PortBindings      map[string][]dockerx.PortBinding `json:"PortBindings,omitempty"`
	RestartPolicy     dockerx.RestartPolicy            `json:"RestartPolicy,omitempty"`
	AutoRemove        bool                             `json:"AutoRemove,omitempty"`
	CapAdd            []string                         `json:"CapAdd,omitempty"`
	CapDrop           []string                         `json:"CapDrop,omitempty"`
	CgroupnsMode      string                           `json:"CgroupnsMode,omitempty"`
	Dns               []string                         `json:"Dns,omitempty"`
	DnsOptions        []string                         `json:"DnsOptions,omitempty"`
	DnsSearch         []string                         `json:"DnsSearch,omitempty"`
	ExtraHosts        []string                         `json:"ExtraHosts,omitempty"`
	GroupAdd          []string                         `json:"GroupAdd,omitempty"`
	IpcMode           string                           `json:"IpcMode,omitempty"`
	Cgroup            string                           `json:"Cgroup,omitempty"`
	Links             []string                         `json:"Links,omitempty"`
	OomScoreAdj       int                              `json:"OomScoreAdj,omitempty"`
	PidMode           string                           `json:"PidMode,omitempty"`
	Privileged        bool                             `json:"Privileged,omitempty"`
	PublishAllPorts   bool                             `json:"PublishAllPorts,omitempty"`
	ReadonlyRootfs    bool                             `json:"ReadonlyRootfs,omitempty"`
	SecurityOpt       []string                         `json:"SecurityOpt,omitempty"`
	StorageOpt        map[string]string                `json:"StorageOpt,omitempty"`
	Tmpfs             map[string]string                `json:"Tmpfs,omitempty"`
	UTSMode           string                           `json:"UTSMode,omitempty"`
	UsernsMode        string                           `json:"UsernsMode,omitempty"`
	ShmSize           int64                            `json:"ShmSize,omitempty"`
	Sysctls           map[string]string                `json:"Sysctls,omitempty"`
	Runtime           string                           `json:"Runtime,omitempty"`
	Isolation         string                           `json:"Isolation,omitempty"`
	Devices           []dockerx.DeviceMapping          `json:"Devices,omitempty"`
	DeviceCgroupRules []string                         `json:"DeviceCgroupRules,omitempty"`
	DeviceRequests    []dockerx.DeviceRequest          `json:"DeviceRequests,omitempty"`
	Init              *bool                            `json:"Init,omitempty"`
	Mounts            []dockerx.MountSpec              `json:"Mounts,omitempty"`
	LogConfig         *dockerx.LogConfig               `json:"LogConfig,omitempty"`

	// Resources
	Memory               int64                    `json:"Memory,omitempty"`
	MemorySwap           int64                    `json:"MemorySwap,omitempty"`
	MemorySwappiness     *int64                   `json:"MemorySwappiness,omitempty"`
	MemoryReservation    int64                    `json:"MemoryReservation,omitempty"`
	KernelMemoryTCP      int64                    `json:"KernelMemoryTCP,omitempty"`
	OomKillDisable       *bool                    `json:"OomKillDisable,omitempty"`
	CPUShares            int64                    `json:"CpuShares,omitempty"`
	CPUQuota             int64                    `json:"CpuQuota,omitempty"`
	CPUPeriod            int64                    `json:"CpuPeriod,omitempty"`
	CPURealtimePeriod    int64                    `json:"CpuRealtimePeriod,omitempty"`
	CPURealtimeRuntime   int64                    `json:"CpuRealtimeRuntime,omitempty"`
	NanoCpus             int64                    `json:"NanoCpus,omitempty"`
	CpusetCpus           string                   `json:"CpusetCpus,omitempty"`
	CpusetMems           string                   `json:"CpusetMems,omitempty"`
	CPUCount             int64                    `json:"CpuCount,omitempty"`
	CPUPercent           int64                    `json:"CpuPercent,omitempty"`
	PidsLimit            *int64                   `json:"PidsLimit,omitempty"`
	BlkioWeight          uint16                   `json:"BlkioWeight,omitempty"`
	BlkioWeightDevice    []dockerx.WeightDevice   `json:"BlkioWeightDevice,omitempty"`
	BlkioDeviceReadBps   []dockerx.ThrottleDevice `json:"BlkioDeviceReadBps,omitempty"`
	BlkioDeviceWriteBps  []dockerx.ThrottleDevice `json:"BlkioDeviceWriteBps,omitempty"`
	BlkioDeviceReadIOps  []dockerx.ThrottleDevice `json:"BlkioDeviceReadIOps,omitempty"`
	BlkioDeviceWriteIOps []dockerx.ThrottleDevice `json:"BlkioDeviceWriteIOps,omitempty"`
	Ulimits              []dockerx.Ulimit         `json:"Ulimits,omitempty"`
}

// AddLabel sets a management label on the spec (management metadata lives in
// the container labels so a replacement container stays self-describing).
func (s *ContainerSpec) AddLabel(key, value string) {
	if s.Config.Labels == nil {
		s.Config.Labels = map[string]string{}
	}
	s.Config.Labels[key] = value
}

// Label reads a management label.
func (s *ContainerSpec) Label(key string) string {
	return s.Config.Labels[key]
}

// CreateRequestBody renders the Engine API POST /containers/create body.
// The spec's Config fields flatten to the top level, matching the API shape.
func CreateRequestBody(spec *ContainerSpec) any {
	return struct {
		ConfigSpec
		HostConfig       *HostConfigSpec           `json:"HostConfig"`
		NetworkingConfig map[string]EndpointConfig `json:"NetworkingConfig,omitempty"`
	}{
		ConfigSpec:       spec.Config,
		HostConfig:       &spec.HostConfig,
		NetworkingConfig: spec.Networks,
	}
}
