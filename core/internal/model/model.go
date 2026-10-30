// Package model defines the wire protocol shared by the daemon and the SwiftUI
// client. Field names are intentionally camelCase to match docs/PROTOCOL.md.
package model

// ContextInfo describes the Docker/OrbStack context currently in use.
type ContextInfo struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Kind     string `json:"kind"`
}

// Reason explains a classification or risk decision.
type Reason struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // info | warning | blocker
}

// PortBinding is a host -> container port mapping.
type PortBinding struct {
	HostIP        string `json:"hostIP"` // "" means all interfaces
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"` // tcp | udp
}

// Binding is the route installed for a managed service.
type Binding struct {
	HostIP        string `json:"hostIP"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	ProxyPort     int    `json:"proxyPort"`
	Protocol      string `json:"protocol"`
}

// ProxyMapping is a listen:target pair inside the proxy container.
type ProxyMapping struct {
	ListenPort int `json:"listenPort"`
	TargetPort int `json:"targetPort"`
}

// Healthcheck summarises the container health configuration.
type Healthcheck struct {
	Configured bool   `json:"configured"`
	Status     string `json:"status"`
	Test       string `json:"test"`
}

// Mount is a bind mount or volume.
type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readOnly"`
}

// Network is an attached Docker network.
type Network struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
}

// Classification is the scan verdict for a container.
type Classification struct {
	Verdict         string        `json:"verdict"` // eligible | needs_review | excluded
	Reasons         []Reason      `json:"reasons"`
	Warnings        []Reason      `json:"warnings"`
	ProxyCandidates []PortBinding `json:"proxyCandidates"`
}

// Container is a scanned container.
type Container struct {
	ID                     string         `json:"id"`
	Name                   string         `json:"name"`
	Image                  string         `json:"image"`
	State                  string         `json:"state"`
	StatusText             string         `json:"statusText"`
	CreatedAt              string         `json:"createdAt"`
	StartedAt              string         `json:"startedAt"`
	ComposeProject         string         `json:"composeProject"`
	ComposeService         string         `json:"composeService"`
	ComposeWorkingDir      string         `json:"composeWorkingDir"`
	ComposeConfigFiles     []string       `json:"composeConfigFiles"`
	ComposeContainerNumber int            `json:"composeContainerNumber"`
	Oneoff                 bool           `json:"oneoff"`
	NetworkMode            string         `json:"networkMode"`
	Networks               []Network      `json:"networks"`
	RestartPolicy          string         `json:"restartPolicy"`
	AutoRemove             bool           `json:"autoRemove"`
	Privileged             bool           `json:"privileged"`
	ReadonlyRootfs         bool           `json:"readonlyRootfs"`
	Healthcheck            Healthcheck    `json:"healthcheck"`
	Mounts                 []Mount        `json:"mounts"`
	Ports                  []PortBinding  `json:"ports"`
	ManagedServiceID       string         `json:"managedServiceId"`
	Classification         Classification `json:"classification"`
}

// ScanResult is returned by the scan method.
type ScanResult struct {
	ScannedAt  string         `json:"scannedAt"`
	Context    ContextInfo    `json:"context"`
	Containers []Container    `json:"containers"`
	Counts     map[string]int `json:"counts"`
}

// PlanItem describes the change for one container.
type PlanItem struct {
	ContainerID             string         `json:"containerId"`
	ContainerName           string         `json:"containerName"`
	ComposeProject          string         `json:"composeProject"`
	ComposeService          string         `json:"composeService"`
	ComposeFile             string         `json:"composeFile"`
	Removals                []PortBinding  `json:"removals"`
	ProxyMappings           []ProxyMapping `json:"proxyMappings"`
	Bindings                []Binding      `json:"bindings"`
	RequiresLanConfirmation bool           `json:"requiresLanConfirmation"`
	Risks                   []Reason       `json:"risks"`
	Diff                    string         `json:"diff"`
	Steps                   []string       `json:"steps"`
}

// Plan is a reviewed attach plan.
type Plan struct {
	ID        string     `json:"id"`
	CreatedAt string     `json:"createdAt"`
	Status    string     `json:"status"` // ready | blocked
	Blockers  []Reason   `json:"blockers"`
	Warnings  []Reason   `json:"warnings"`
	Items     []PlanItem `json:"items"`
}

// LogEntry is one line in an operation log.
type LogEntry struct {
	At      string `json:"at"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Operation is a long-running or recorded action.
type Operation struct {
	ID                 string     `json:"id"`
	Kind               string     `json:"kind"`
	Status             string     `json:"status"` // running | succeeded | failed | rolled_back | rollback_failed
	StartedAt          string     `json:"startedAt"`
	FinishedAt         string     `json:"finishedAt"`
	Error              string     `json:"error"`
	BackupDir          string     `json:"backupDir"`
	AffectedServiceIDs []string   `json:"affectedServiceIds"`
	Log                []LogEntry `json:"log"`
}

// Service is a managed (sleepable) service.
type Service struct {
	ID                string    `json:"id"`
	ContainerID       string    `json:"containerId"`
	ContainerName     string    `json:"containerName"`
	ComposeProject    string    `json:"composeProject"`
	ComposeService    string    `json:"composeService"`
	ComposeFile       string    `json:"composeFile"`
	Status            string    `json:"status"` // managed_running | managed_idle | managed_stopped | missing | warning | failed
	Bindings          []Binding `json:"bindings"`
	IdleTimeoutSecs   int       `json:"idleTimeoutSecs"`
	StartTimeoutSecs  int       `json:"startTimeoutSecs"`
	ActiveConnections int       `json:"activeConnections"`
	LastActiveAt      string    `json:"lastActiveAt"`
	CreatedAt         string    `json:"createdAt"`
	LastError         string    `json:"lastError"`
	ExcludedFromSleep bool      `json:"excludedFromSleep"`
	// DockerSocket pins the service to the Docker instance it was attached on
	// (unix socket path without the unix:// prefix). Empty for records created
	// before this field existed; reconcile never rebinds a record to a
	// different socket by name.
	DockerSocket string `json:"dockerSocket,omitempty"`
}

// Range is an inclusive port range.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ProxyStatus describes the Lazy TCP Proxy container.
type ProxyStatus struct {
	Installed        bool   `json:"installed"`
	Running          bool   `json:"running"`
	AdminReachable   bool   `json:"adminReachable"`
	MetricsReachable bool   `json:"metricsReachable"`
	ContainerName    string `json:"containerName"`
	ContainerIP      string `json:"containerIP"`
	PublishedRange   Range  `json:"publishedRange"`
	Error            string `json:"error"`
}

// Counts summarises managed and discoverable containers.
type Counts struct {
	Managed     int `json:"managed"`
	Running     int `json:"running"`
	Idle        int `json:"idle"`
	Stopped     int `json:"stopped"`
	Missing     int `json:"missing"`
	Eligible    int `json:"eligible"`
	NeedsReview int `json:"needsReview"`
	Excluded    int `json:"excluded"`
}

// LastError is the most recent daemon-level error.
type LastError struct {
	At      string `json:"at"`
	Message string `json:"message"`
}

// Status is the top-level dashboard state.
type Status struct {
	DaemonVersion        string      `json:"daemonVersion"`
	DataDir              string      `json:"dataDir"`
	Context              ContextInfo `json:"context"`
	DockerReachable      bool        `json:"dockerReachable"`
	Proxy                ProxyStatus `json:"proxy"`
	Counts               Counts      `json:"counts"`
	LastScanAt           string      `json:"lastScanAt"`
	LastError            LastError   `json:"lastError"`
	LaunchAgentInstalled bool        `json:"launchAgentInstalled"`
}

// Settings is the user configuration exposed by the protocol.
type Settings struct {
	IdleTimeoutSecs      int      `json:"idleTimeoutSecs"`
	StartTimeoutSecs     int      `json:"startTimeoutSecs"`
	ProxyPortStart       int      `json:"proxyPortStart"`
	ProxyPortEnd         int      `json:"proxyPortEnd"`
	AdminPort            int      `json:"adminPort"`
	WebPort              int      `json:"webPort"`
	AutoScanOnLaunch     bool     `json:"autoScanOnLaunch"`
	EnableLowPortHelper  bool     `json:"enableLowPortHelper"`
	LogRetentionDays     int      `json:"logRetentionDays"`
	BackupDir            string   `json:"backupDir"`
	DataDir              string   `json:"dataDir"`
	SocketPath           string   `json:"socketPath"`
	ProxyDialMode        string   `json:"proxyDialMode"` // published | container_ip
	ExcludeImagePatterns []string `json:"excludeImagePatterns"`
	DockerSocket         string   `json:"dockerSocket"` // optional explicit socket; empty = auto detect
	LaunchAgentInstalled bool     `json:"launchAgentInstalled"`
}

// DoctorCheck is one preflight result.
type DoctorCheck struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"` // ok | warn | fail
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// Health is the compact runtime health payload used by the GUI's service and
// diagnostics centers.
type Health struct {
	DaemonVersion       string      `json:"daemonVersion"`
	ProtocolVersion     int         `json:"protocolVersion"`
	DataDir             string      `json:"dataDir"`
	SocketPath          string      `json:"socketPath"`
	UptimeSecs          int64       `json:"uptimeSecs"`
	DockerReachable     bool        `json:"dockerReachable"`
	DockerContext       ContextInfo `json:"dockerContext"`
	Proxy               ProxyStatus `json:"proxy"`
	Counts              Counts      `json:"counts"`
	PendingTransactions int         `json:"pendingTransactions"`
	// Transactions whose recovery failed (rollback_failed / needs_repair):
	// invisible to auto-recovery, must surface in the GUI and block uninstall.
	NeedsRepairTransactions int       `json:"needsRepairTransactions"`
	RunningOperations       int       `json:"runningOperations"`
	RestoreFailed           bool      `json:"restoreFailed"`
	LaunchAgentLoaded       bool      `json:"launchAgentLoaded"`
	LegacyLaunchAgent       bool      `json:"legacyLaunchAgent"`
	LegacyBinaries          []string  `json:"legacyBinaries"`
	LastError               LastError `json:"lastError"`
}

// RecoveryTransaction is a durable in-flight transaction exposed to the GUI.
type RecoveryTransaction struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	StartedAt   string `json:"startedAt"`
	UpdatedAt   string `json:"updatedAt"`
	Error       string `json:"error"`
}

// RecoveryList is returned by system.recovery.list/resolve.
type RecoveryList struct {
	Transactions []RecoveryTransaction `json:"transactions"`
	Resolved     int                   `json:"resolved"`
	Message      string                `json:"message"`
}

// ShutdownPreparation reports whether it is safe to stop/upgrade the daemon.
type ShutdownPreparation struct {
	Safe                bool                  `json:"safe"`
	RunningOperations   []Operation           `json:"runningOperations"`
	PendingTransactions []RecoveryTransaction `json:"pendingTransactions"`
	Message             string                `json:"message"`
}

// DockerContext is one discoverable Docker endpoint.
type DockerContext struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Kind      string `json:"kind"`
	Current   bool   `json:"current"`
	Reachable bool   `json:"reachable"`
	Source    string `json:"source"` // env | orbstack | docker-desktop | context | default
}

// DockerContexts is returned by docker.contexts.
type DockerContexts struct {
	Contexts []DockerContext `json:"contexts"`
	Selected string          `json:"selected"`
}

// BackupEntry describes one manual backup archive.
type BackupEntry struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt"`
	SizeBytes int64  `json:"sizeBytes"`
	Kind      string `json:"kind"` // manual | operation | migration
	Note      string `json:"note"`
}

// BackupsResult is returned by backup.list.
type BackupsResult struct {
	Backups []BackupEntry `json:"backups"`
}

// BackupCreateResult is returned by backup.create.
type BackupCreateResult struct {
	Backup BackupEntry `json:"backup"`
}

// LogsExportResult is returned by logs.export.
type LogsExportResult struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// RestoreResult is returned by backup.restore.
type RestoreResult struct {
	OK              bool   `json:"ok"`
	RestartRequired bool   `json:"restartRequired"`
	Message         string `json:"message"`
	BackupPath      string `json:"backupPath"`
}
