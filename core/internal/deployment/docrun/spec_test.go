package docrun

import (
	"encoding/json"
	"strings"
	"testing"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// goldenInspect mirrors a real `docker inspect` of a docker run container:
// binds, named volume, anonymous volume, custom network, env, labels,
// restart policy, resource limits, healthcheck, port bindings.
const goldenInspect = `{
  "Id": "b1e0f00dcafe1234567890abcdef1234567890abcdef1234567890abcdef1234",
  "Name": "/webapp",
  "Created": "2026-11-01T10:00:00.000000000Z",
  "Path": "docker-entrypoint.sh",
  "Args": ["nginx", "-g", "daemon off;"],
  "State": {"Status": "running", "Running": true, "StartedAt": "2026-11-01T10:00:01Z"},
  "Config": {
    "Hostname": "b1e0f00dcafe",
    "User": "0:0",
    "Env": ["PATH=/usr/local/sbin:/usr/local/bin", "NGINX_VERSION=1.27", "SECRET=from-run"],
    "Cmd": ["nginx", "-g", "daemon off;"],
    "Image": "nginx:1.27",
    "Volumes": {"/var/cache/nginx": {}},
    "WorkingDir": "/srv",
    "Labels": {"team": "platform", "orbsleep.managed": "leftover"},
    "ExposedPorts": {"80/tcp": {}, "8443/tcp": {}},
    "StopSignal": "SIGQUIT",
    "Tty": true,
    "Healthcheck": {"Test": ["CMD-SHELL", "curl -f http://localhost/ || exit 1"], "Interval": 30000000000, "Timeout": 5000000000, "Retries": 3}
  },
  "Image": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
  "HostConfig": {
    "Binds": ["/Users/demo/site:/usr/share/nginx/html:ro", "appdata:/data"],
    "NetworkMode": "webnet",
    "PortBindings": {
      "80/tcp": [{"HostIp": "127.0.0.1", "HostPort": "18080"}],
      "8443/tcp": [{"HostIp": "", "HostPort": "18443"}],
      "53/udp": [{"HostIp": "", "HostPort": "1053"}]
    },
    "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
    "AutoRemove": false,
    "Privileged": false,
    "ReadonlyRootfs": true,
    "CapAdd": ["NET_ADMIN"],
    "CapDrop": ["CHOWN"],
    "SecurityOpt": ["no-new-privileges"],
    "Devices": [{"PathOnHost": "/dev/null", "PathInContainer": "/dev/xnull", "CgroupPermissions": "rwm"}],
    "Dns": ["1.1.1.1"],
    "ExtraHosts": ["db:host-gateway"],
    "GroupAdd": ["999"],
    "IpcMode": "private",
    "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m"}},
    "Memory": 536870912,
    "NanoCpus": 1500000000,
    "PidsLimit": 256,
    "ShmSize": 67108864,
    "Tmpfs": {"/tmp": "rw,noexec"},
    "Sysctls": {"net.ipv4.ip_unprivileged_port_start": "80"},
    "Ulimits": [{"Name": "nofile", "Soft": 8192, "Hard": 16384}],
    "OomScoreAdj": 250,
    "Init": true,
    "CgroupnsMode": "private",
    "Mounts": [{"Type": "volume", "Source": "cfgvol", "Target": "/etc/nginx/conf.d", "ReadOnly": true}]
  },
  "NetworkSettings": {
    "Networks": {
      "webnet": {
        "IPAddress": "172.20.0.5",
        "Gateway": "172.20.0.1",
        "Aliases": ["web", "b1e0f00dcafe"],
        "IPAMConfig": {"IPv4Address": "172.20.0.5"}
      },
      "backnet": {
        "IPAddress": "172.21.0.9",
        "Gateway": "172.21.0.1",
        "Aliases": ["web-internal"]
      }
    },
    "Ports": {}
  },
  "Mounts": [
    {"Type": "bind", "Source": "/Users/demo/site", "Destination": "/usr/share/nginx/html", "RW": false},
    {"Type": "volume", "Name": "appdata", "Source": "/var/lib/docker/volumes/appdata/_data", "Destination": "/data", "RW": true},
    {"Type": "volume", "Name": "9f8e7d6c5b4a3210fedcba9876543210fedcba9876543210fedcba9876543210", "Source": "/var/lib/docker/volumes/9f8e/_data", "Destination": "/var/cache/nginx", "RW": true},
    {"Type": "volume", "Name": "cfgvol", "Source": "/var/lib/docker/volumes/cfgvol/_data", "Destination": "/etc/nginx/conf.d", "RW": false}
  ]
}`

func loadGolden(t *testing.T) *dockerx.ContainerInspect {
	t.Helper()
	var ins dockerx.ContainerInspect
	if err := json.Unmarshal([]byte(goldenInspect), &ins); err != nil {
		t.Fatalf("fixture 解析失败: %v", err)
	}
	return &ins
}

func TestSpecFromInspectGolden(t *testing.T) {
	ins := loadGolden(t)
	spec, err := SpecFromInspect(ins)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "webapp" {
		t.Fatalf("name = %s", spec.Name)
	}
	// Identity-critical config preserved verbatim.
	if spec.Config.Image != "nginx:1.27" {
		t.Fatalf("image = %s", spec.Config.Image)
	}
	if spec.Config.User != "0:0" || spec.Config.WorkingDir != "/srv" || spec.Config.StopSignal != "SIGQUIT" || !spec.Config.Tty {
		t.Fatalf("basic config lost: %+v", spec.Config)
	}
	if len(spec.Config.Cmd) != 3 || spec.Config.Cmd[1] != "-g" {
		t.Fatalf("cmd lost: %v", spec.Config.Cmd)
	}
	if spec.Config.Healthcheck == nil || len(spec.Config.Healthcheck.Test) != 2 {
		t.Fatalf("healthcheck lost: %+v", spec.Config.Healthcheck)
	}
	// Auto hostname (12-hex short id) must be dropped, not pinned.
	if spec.Config.Hostname != "" {
		t.Fatalf("auto hostname must not be pinned, got %q", spec.Config.Hostname)
	}
	// HostConfig preservation. Binds carry the two user binds plus the
	// remapped anonymous volume (see below).
	hc := spec.HostConfig
	if len(hc.Binds) != 3 || hc.Binds[0] != "/Users/demo/site:/usr/share/nginx/html:ro" || hc.Binds[1] != "appdata:/data" {
		t.Fatalf("binds lost: %v", hc.Binds)
	}
	if hc.NetworkMode != "webnet" || !hc.ReadonlyRootfs || hc.Memory != 536870912 || hc.NanoCpus != 1500000000 {
		t.Fatalf("hostconfig lost: %+v", hc)
	}
	if hc.RestartPolicy.Name != "unless-stopped" || len(hc.CapAdd) != 1 || hc.CapAdd[0] != "NET_ADMIN" {
		t.Fatalf("restart/caps lost: %+v", hc.RestartPolicy)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges" {
		t.Fatalf("securityopt lost: %v", hc.SecurityOpt)
	}
	if len(hc.Devices) != 1 || hc.Devices[0].PathOnHost != "/dev/null" {
		t.Fatalf("devices lost: %+v", hc.Devices)
	}
	if hc.LogConfig == nil || hc.LogConfig.Type != "json-file" || hc.LogConfig.Config["max-size"] != "10m" {
		t.Fatalf("logconfig lost: %+v", hc.LogConfig)
	}
	if len(hc.Ulimits) != 1 || hc.Ulimits[0].Hard != 16384 {
		t.Fatalf("ulimits lost: %+v", hc.Ulimits)
	}
	if hc.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "80" {
		t.Fatalf("sysctls lost: %+v", hc.Sysctls)
	}
	if hc.Tmpfs["/tmp"] != "rw,noexec" {
		t.Fatalf("tmpfs lost: %+v", hc.Tmpfs)
	}
	if hc.Init == nil || !*hc.Init {
		t.Fatalf("init lost: %+v", hc.Init)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Source != "cfgvol" {
		t.Fatalf("mounts lost: %+v", hc.Mounts)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 256 {
		t.Fatalf("pidslimit lost: %+v", hc.PidsLimit)
	}
	// Port bindings preserved at plan time (attach strips takeovers later).
	if len(hc.PortBindings) != 3 {
		t.Fatalf("port bindings lost: %+v", hc.PortBindings)
	}
	// Anonymous volume remapped by name into Binds.
	foundAnon := false
	for _, b := range hc.Binds {
		if strings.HasPrefix(b, "9f8e7d6c5b4a3210") {
			foundAnon = true
			if !strings.HasSuffix(b, ":/var/cache/nginx") {
				t.Fatalf("anonymous volume bind wrong target: %s", b)
			}
		}
	}
	if !foundAnon {
		t.Fatalf("anonymous volume not remapped: %v", hc.Binds)
	}
	// Networking: primary network carried with aliases + static IP.
	ep, ok := spec.Networks["webnet"]
	if !ok {
		t.Fatalf("primary network missing: %+v", spec.Networks)
	}
	// "b1e0f00dcafe" is the container's own short-ID identity and must be
	// filtered; only the user alias survives.
	if len(ep.Aliases) != 1 || ep.Aliases[0] != "web" || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "172.20.0.5" {
		t.Fatalf("endpoint config lost: %+v", ep)
	}
	if len(ep.DNSNames) != 1 || ep.DNSNames[0] != "web" {
		t.Fatalf("DNSNames must mirror aliases: %+v", ep.DNSNames)
	}
	// backnet must NOT be in the create payload (connected post-create).
	if _, ok := spec.Networks["backnet"]; ok {
		t.Fatalf("secondary network must not be in create payload")
	}
	// Secondary list contains backnet.
	if len(SecondaryNetworks(ins)) != 1 || SecondaryNetworks(ins)[0] != "backnet" {
		t.Fatalf("secondary networks wrong: %v", SecondaryNetworks(ins))
	}
	// User labels preserved; orbsleep labels are additive only.
	if spec.Config.Labels["team"] != "platform" {
		t.Fatalf("user labels lost: %+v", spec.Config.Labels)
	}
}

func TestPrepareAttachStripsOnlyTakeoverPorts(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp", "8443/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prep.ManagedSpec.HostConfig.PortBindings) != 1 {
		t.Fatalf("managed spec must keep only UDP binding: %+v", prep.ManagedSpec.HostConfig.PortBindings)
	}
	if _, ok := prep.ManagedSpec.HostConfig.PortBindings["53/udp"]; !ok {
		t.Fatalf("udp binding must survive: %+v", prep.ManagedSpec.HostConfig.PortBindings)
	}
	// Original spec keeps everything.
	if len(prep.OriginalSpec.HostConfig.PortBindings) != 3 {
		t.Fatalf("original spec must keep all bindings: %+v", prep.OriginalSpec.HostConfig.PortBindings)
	}
	// Management labels on the managed spec only.
	if prep.ManagedSpec.Config.Labels[LabelManaged] != "true" ||
		prep.ManagedSpec.Config.Labels[LabelServiceID] != "svc-1" {
		t.Fatalf("management labels missing: %+v", prep.ManagedSpec.Config.Labels)
	}
	// The original spec preserves the container's own labels verbatim — even
	// a stale orbsleep label from an earlier cycle (fingerprinting ignores
	// orbsleep.* either way).
	if prep.OriginalSpec.Config.Labels["orbsleep.managed"] != "leftover" {
		t.Fatalf("original labels must be preserved verbatim: %+v", prep.OriginalSpec.Config.Labels)
	}
	// Deep copy: mutating managed must not affect original.
	if len(prep.OriginalSpec.HostConfig.PortBindings["80/tcp"]) == 0 {
		t.Fatal("original spec was mutated through the managed copy")
	}
	// Backup name deterministic.
	want := "webapp.orbsleep-bak-b1e0f00dcafe"
	if prep.BackupName != want {
		t.Fatalf("backup name = %s, want %s", prep.BackupName, want)
	}
	// ExposedPorts stays consistent on the managed spec.
	if _, ok := prep.ManagedSpec.Config.ExposedPorts["53/udp"]; !ok {
		t.Fatal("udp exposed port must survive")
	}
}

func TestFingerprintIgnoresManagementLabels(t *testing.T) {
	ins := loadGolden(t)
	a, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := PrepareAttach(ins, "svc-2", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	// Same container, different service id → same fingerprint.
	if Fingerprint(a.OriginalSpec) != Fingerprint(b.OriginalSpec) {
		t.Fatal("fingerprint must ignore management labels")
	}
	// A real config change must change the fingerprint.
	ins.Config.Env = append(ins.Config.Env, "NEW=1")
	c, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.OriginalSpec) == Fingerprint(c.OriginalSpec) {
		t.Fatal("fingerprint must react to env changes")
	}
}

func TestVerifyUnchangedDetectsDrift(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{prep.OriginalID: ins}}
	if err := VerifyUnchanged(t.Context(), eng, prep); err != nil {
		t.Fatalf("unchanged container must pass: %v", err)
	}
	// Simulate an external edit after the plan.
	drifted := loadGolden(t)
	drifted.Config.WorkingDir = "/elsewhere"
	eng2 := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{prep.OriginalID: drifted}}
	if err := VerifyUnchanged(t.Context(), eng2, prep); err == nil {
		t.Fatal("drifted container must be refused")
	}
}

func TestRemoveTCPPortBindingsKeepsUDP(t *testing.T) {
	spec := &runtime.ContainerSpec{HostConfig: runtime.HostConfigSpec{
		PortBindings: map[string][]dockerx.PortBinding{
			"80/tcp":   {{HostPort: "80"}},
			"53/udp":   {{HostPort: "53"}},
			"9090/tcp": {{HostPort: "9090"}},
		},
	}}
	RemoveTCPPortBindings(spec, map[string]struct{}{"80/tcp": {}, "9090/tcp": {}})
	if len(spec.HostConfig.PortBindings) != 1 {
		t.Fatalf("bindings after removal: %+v", spec.HostConfig.PortBindings)
	}
	if _, ok := spec.HostConfig.PortBindings["53/udp"]; !ok {
		t.Fatal("udp must survive")
	}
}
