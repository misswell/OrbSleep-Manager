package dockerx

// ContainerSummary is one entry from GET /containers/json?all=1.
type ContainerSummary struct {
	ID              string            `json:"Id"`
	Names           []string          `json:"Names"`
	Image           string            `json:"Image"`
	ImageID         string            `json:"ImageID"`
	Command         string            `json:"Command"`
	Created         int64             `json:"Created"`
	State           string            `json:"State"`
	Status          string            `json:"Status"`
	Labels          map[string]string `json:"Labels"`
	Ports           []PortSummary     `json:"Ports"`
	Mounts          []MountPoint      `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
}

// PortSummary is a published port in the list payload.
type PortSummary struct {
	IP          string `json:"IP"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"`
}

// MountPoint describes a mount.
type MountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
	Driver      string `json:"Driver"`
	Mode        string `json:"Mode"`
	Propagation string `json:"Propagation"`
}

// EndpointSettings is a container network endpoint.
type EndpointSettings struct {
	IPAddress  string      `json:"IPAddress"`
	Gateway    string      `json:"Gateway"`
	NetworkID  string      `json:"NetworkID"`
	Aliases    []string    `json:"Aliases"`
	IPAMConfig *IPAMConfig `json:"IPAMConfig,omitempty"`
	DriverOpts []string    `json:"DriverOpts,omitempty"`
	MacAddress string      `json:"MacAddress,omitempty"`
}

// IPAMConfig carries user-assigned static addresses for one endpoint.
type IPAMConfig struct {
	IPv4Address  string   `json:"IPv4Address,omitempty"`
	IPv6Address  string   `json:"IPv6Address,omitempty"`
	LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
}

// HealthState is the runtime health block of an inspected container.
type HealthState struct {
	Status        string `json:"Status"`
	FailingStreak int    `json:"FailingStreak"`
	Log           []any  `json:"Log,omitempty"`
}

// Healthcheck is the healthcheck definition (inspect + create payloads).
type Healthcheck struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
}

// PortBinding is a host binding from the Docker API.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy describes what happens when a container exits.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

// DeviceMapping is one device passthrough request.
type DeviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

// DeviceRequest is a GPU/device driver request (copied verbatim on recreate).
type DeviceRequest struct {
	Driver       string            `json:"Driver,omitempty"`
	Count        int               `json:"Count,omitempty"`
	DeviceIDs    []string          `json:"DeviceIDs,omitempty"`
	Capabilities [][]string        `json:"Capabilities,omitempty"`
	Options      map[string]string `json:"Options,omitempty"`
}

// LogConfig is the logging driver configuration.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

// Ulimit is one rlimit applied to the container.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// MountSpec is one requested mount from HostConfig.Mounts (create payload shape).
type MountSpec struct {
	Type          string         `json:"Type"` // bind | volume | tmpfs | npipe | cluster
	Source        string         `json:"Source,omitempty"`
	Target        string         `json:"Target"`
	ReadOnly      bool           `json:"ReadOnly,omitempty"`
	Consistency   string         `json:"Consistency,omitempty"`
	BindOptions   *BindOptions   `json:"BindOptions,omitempty"`
	VolumeOptions *VolumeOptions `json:"VolumeOptions,omitempty"`
	TmpfsOptions  *TmpfsOptions  `json:"TmpfsOptions,omitempty"`
}

// BindOptions refines a bind mount.
type BindOptions struct {
	Propagation            string `json:"Propagation,omitempty"`
	NonRecursive           bool   `json:"NonRecursive,omitempty"`
	CreateMountpoint       bool   `json:"CreateMountpoint,omitempty"`
	ReadOnlyNonRecursive   bool   `json:"ReadOnlyNonRecursive,omitempty"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive,omitempty"`
}

// VolumeOptions refines a volume mount.
type VolumeOptions struct {
	NoCopy       bool              `json:"NoCopy,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	DriverConfig map[string]string `json:"DriverConfig,omitempty"`
}

// TmpfsOptions refines a tmpfs mount.
type TmpfsOptions struct {
	SizeBytes int64 `json:"SizeBytes,omitempty"`
	Mode      int   `json:"Mode,omitempty"`
}

// ContainerInspect is the GET /containers/{id}/json payload subset OrbSleep
// understands. It carries every field the docker-run recreate path needs to
// rebuild a container losslessly; runtime-only state (IDs, IPs, counters) is
// deliberately excluded from the recreate mapping in the deployment package.
type ContainerInspect struct {
	ID      string   `json:"Id"`
	Name    string   `json:"Name"`
	Created string   `json:"Created"`
	Path    string   `json:"Path"`
	Args    []string `json:"Args"`
	State   struct {
		Status     string       `json:"Status"`
		Running    bool         `json:"Running"`
		Paused     bool         `json:"Paused"`
		Restarting bool         `json:"Restarting"`
		OOMKilled  bool         `json:"OOMKilled"`
		Dead       bool         `json:"Dead"`
		Pid        int          `json:"Pid"`
		ExitCode   int          `json:"ExitCode"`
		StartedAt  string       `json:"StartedAt"`
		FinishedAt string       `json:"FinishedAt"`
		Health     *HealthState `json:"Health,omitempty"`
	} `json:"State"`
	Config struct {
		Hostname     string              `json:"Hostname,omitempty"`
		Domainname   string              `json:"Domainname,omitempty"`
		User         string              `json:"User,omitempty"`
		AttachStdin  bool                `json:"AttachStdin,omitempty"`
		AttachStdout bool                `json:"AttachStdout,omitempty"`
		AttachStderr bool                `json:"AttachStderr,omitempty"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
		Tty          bool                `json:"Tty,omitempty"`
		OpenStdin    bool                `json:"OpenStdin,omitempty"`
		StdinOnce    bool                `json:"StdinOnce,omitempty"`
		Env          []string            `json:"Env,omitempty"`
		Cmd          []string            `json:"Cmd,omitempty"`
		Healthcheck  *Healthcheck        `json:"Healthcheck,omitempty"`
		Image        string              `json:"Image"`
		Volumes      map[string]struct{} `json:"Volumes,omitempty"`
		WorkingDir   string              `json:"WorkingDir,omitempty"`
		Entrypoint   []string            `json:"Entrypoint,omitempty"`
		OnBuild      []string            `json:"OnBuild,omitempty"`
		Labels       map[string]string   `json:"Labels,omitempty"`
		StopSignal   string              `json:"StopSignal,omitempty"`
		StopTimeout  *int                `json:"StopTimeout,omitempty"`
	} `json:"Config"`
	Image      string `json:"Image"`
	HostConfig struct {
		Binds                []string                 `json:"Binds,omitempty"`
		ContainerIDFile      string                   `json:"ContainerIDFile,omitempty"`
		LogConfig            LogConfig                `json:"LogConfig"`
		NetworkMode          string                   `json:"NetworkMode"`
		PortBindings         map[string][]PortBinding `json:"PortBindings"`
		RestartPolicy        RestartPolicy            `json:"RestartPolicy"`
		AutoRemove           bool                     `json:"AutoRemove"`
		VolumeDriver         string                   `json:"VolumeDriver,omitempty"`
		VolumesFrom          []string                 `json:"VolumesFrom,omitempty"`
		CapAdd               []string                 `json:"CapAdd,omitempty"`
		CapDrop              []string                 `json:"CapDrop,omitempty"`
		CgroupnsMode         string                   `json:"CgroupnsMode,omitempty"`
		Dns                  []string                 `json:"Dns,omitempty"`
		DnsOptions           []string                 `json:"DnsOptions,omitempty"`
		DnsSearch            []string                 `json:"DnsSearch,omitempty"`
		ExtraHosts           []string                 `json:"ExtraHosts,omitempty"`
		GroupAdd             []string                 `json:"GroupAdd,omitempty"`
		IpcMode              string                   `json:"IpcMode,omitempty"`
		Cgroup               string                   `json:"Cgroup,omitempty"`
		Links                []string                 `json:"Links,omitempty"`
		OomScoreAdj          int                      `json:"OomScoreAdj,omitempty"`
		PidMode              string                   `json:"PidMode,omitempty"`
		Privileged           bool                     `json:"Privileged"`
		PublishAllPorts      bool                     `json:"PublishAllPorts,omitempty"`
		ReadonlyRootfs       bool                     `json:"ReadonlyRootfs"`
		SecurityOpt          []string                 `json:"SecurityOpt,omitempty"`
		StorageOpt           map[string]string        `json:"StorageOpt,omitempty"`
		Tmpfs                map[string]string        `json:"Tmpfs,omitempty"`
		UTSMode              string                   `json:"UTSMode,omitempty"`
		UsernsMode           string                   `json:"UsernsMode,omitempty"`
		ShmSize              int64                    `json:"ShmSize,omitempty"`
		Sysctls              map[string]string        `json:"Sysctls,omitempty"`
		Runtime              string                   `json:"Runtime,omitempty"`
		Isolation            string                   `json:"Isolation,omitempty"`
		Devices              []DeviceMapping          `json:"Devices,omitempty"`
		DeviceCgroupRules    []string                 `json:"DeviceCgroupRules,omitempty"`
		DeviceRequests       []DeviceRequest          `json:"DeviceRequests,omitempty"`
		Init                 *bool                    `json:"Init,omitempty"`
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
		BlkioWeightDevice    []*WeightDevice          `json:"BlkioWeightDevice,omitempty"`
		BlkioDeviceReadBps   []*ThrottleDevice        `json:"BlkioDeviceReadBps,omitempty"`
		BlkioDeviceWriteBps  []*ThrottleDevice        `json:"BlkioDeviceWriteBps,omitempty"`
		BlkioDeviceReadIOps  []*ThrottleDevice        `json:"BlkioDeviceReadIOps,omitempty"`
		BlkioDeviceWriteIOps []*ThrottleDevice        `json:"BlkioDeviceWriteIOps,omitempty"`
		Ulimits              []Ulimit                 `json:"Ulimits,omitempty"`
		Mounts               []MountSpec              `json:"Mounts,omitempty"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
		Ports    map[string][]PortBinding    `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []MountPoint `json:"Mounts"`
}

// WeightDevice is a blkio weight per device.
type WeightDevice struct {
	Path   string `json:"Path"`
	Weight uint16 `json:"Weight"`
}

// ThrottleDevice is a blkio rate limit per device.
type ThrottleDevice struct {
	Path string `json:"Path"`
	Rate uint64 `json:"Rate"`
}

// WeightDeviceList and ThrottleDeviceList convert pointers for spec copies.
func CopyWeightDevices(in []*WeightDevice) []WeightDevice {
	if len(in) == 0 {
		return nil
	}
	out := make([]WeightDevice, len(in))
	for i, d := range in {
		if d != nil {
			out[i] = *d
		}
	}
	return out
}

// CopyThrottleDevices dereferences the API's pointer-slice shape.
func CopyThrottleDevices(in []*ThrottleDevice) []ThrottleDevice {
	if len(in) == 0 {
		return nil
	}
	out := make([]ThrottleDevice, len(in))
	for i, d := range in {
		if d != nil {
			out[i] = *d
		}
	}
	return out
}

// EngineVersion identifies the engine behind a socket.
type EngineVersion struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
	Components []struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	} `json:"Components"`
}

// IsAnonymousVolume reports whether a mounted volume looks auto-generated
// (64 hex chars) rather than a user-named volume.
func IsAnonymousVolume(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
