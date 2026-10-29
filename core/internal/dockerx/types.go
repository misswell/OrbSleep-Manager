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
}

// EndpointSettings is a container network endpoint.
type EndpointSettings struct {
	IPAddress string   `json:"IPAddress"`
	Gateway   string   `json:"Gateway"`
	NetworkID string   `json:"NetworkID"`
	Aliases   []string `json:"Aliases"`
}

// ContainerInspect is the subset of GET /containers/{id}/json we use.
type ContainerInspect struct {
	ID      string   `json:"Id"`
	Name    string   `json:"Name"`
	Created string   `json:"Created"`
	Path    string   `json:"Path"`
	Args    []string `json:"Args"`
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image       string            `json:"Image"`
		Labels      map[string]string `json:"Labels"`
		Healthcheck *struct {
			Test        []string `json:"Test"`
			Interval    int64    `json:"Interval"`
			Timeout     int64    `json:"Timeout"`
			Retries     int      `json:"Retries"`
			StartPeriod int64    `json:"StartPeriod"`
		} `json:"Healthcheck"`
		Env []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings  map[string][]PortBinding `json:"PortBindings"`
		NetworkMode   string                   `json:"NetworkMode"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		AutoRemove     bool     `json:"AutoRemove"`
		Privileged     bool     `json:"Privileged"`
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		CapAdd         []string `json:"CapAdd"`
		Devices        []struct {
			PathOnHost        string `json:"PathOnHost"`
			PathInContainer   string `json:"PathInContainer"`
			CgroupPermissions string `json:"CgroupPermissions"`
		} `json:"Devices"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
		Ports    map[string][]PortBinding    `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []MountPoint `json:"Mounts"`
}

// PortBinding is a host binding from the Docker API.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}
