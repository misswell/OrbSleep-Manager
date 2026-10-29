package scan

import (
	"os"
	"path/filepath"
	"testing"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
)

func baseInspect(t *testing.T) *dockerx.ContainerInspect {
	t.Helper()
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services:\n  web:\n    image: nginx:alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ins := &dockerx.ContainerInspect{ID: "abc123", Name: "/demo-web-1"}
	ins.Config.Image = "nginx:alpine"
	ins.Config.Labels = map[string]string{
		"com.docker.compose.project":              "demo",
		"com.docker.compose.service":              "web",
		"com.docker.compose.container-number":     "1",
		"com.docker.compose.project.working_dir":  dir,
		"com.docker.compose.project.config_files": composeFile,
	}
	ins.State.Status = "running"
	ins.HostConfig.NetworkMode = "demo_default"
	ins.HostConfig.RestartPolicy.Name = "unless-stopped"
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{
		"80/tcp": {{HostIP: "127.0.0.1", HostPort: "18080"}},
	}
	ins.NetworkSettings.Networks = map[string]dockerx.EndpointSettings{
		"demo_default": {IPAddress: "172.20.0.2", Gateway: "172.20.0.1"},
	}
	return ins
}

func TestEligibleComposeService(t *testing.T) {
	c := Convert(baseInspect(t), Options{})
	if c.Classification.Verdict != "eligible" {
		t.Fatalf("verdict = %s, reasons=%+v warnings=%+v", c.Classification.Verdict, c.Classification.Reasons, c.Classification.Warnings)
	}
	if len(c.Ports) != 1 || c.Ports[0].HostPort != 18080 {
		t.Fatalf("ports = %+v", c.Ports)
	}
	if len(c.Classification.ProxyCandidates) != 1 {
		t.Fatalf("candidates = %+v", c.Classification.ProxyCandidates)
	}
}

func TestHostNetworkExcluded(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.NetworkMode = "host"
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" {
		t.Fatalf("verdict = %s", c.Classification.Verdict)
	}
	if !hasReason(c.Classification, "network_mode_host") {
		t.Fatal("missing network_mode_host reason")
	}
}

func TestNoComposeExcluded(t *testing.T) {
	ins := baseInspect(t)
	ins.Config.Labels = map[string]string{}
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "no_compose") {
		t.Fatalf("verdict = %s reasons=%+v", c.Classification.Verdict, c.Classification.Reasons)
	}
}

func TestLowPortNeedsHelper(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "80"}}}
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "low_port") {
		t.Fatalf("verdict = %s reasons=%+v", c.Classification.Verdict, c.Classification.Reasons)
	}
	c = Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "low_port") {
		t.Fatalf("low port must stay excluded until a privileged helper exists: %+v", c.Classification.Reasons)
	}
}

func TestAllInterfacesNeedsReview(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{"80/tcp": {{HostIP: "", HostPort: "18080"}}}
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "needs_review" {
		t.Fatalf("verdict = %s", c.Classification.Verdict)
	}
	if len(c.Classification.Warnings) == 0 || c.Classification.Warnings[0].Code != "all_interfaces" {
		t.Fatalf("warnings = %+v", c.Classification.Warnings)
	}
}

func TestIPv6BindingExcluded(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{"80/tcp": {{HostIP: "::1", HostPort: "18080"}}}
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "ipv6_binding") {
		t.Fatalf("verdict = %s reasons=%+v", c.Classification.Verdict, c.Classification.Reasons)
	}
}

func TestStatefulImageNeedsReview(t *testing.T) {
	ins := baseInspect(t)
	ins.Config.Image = "postgres:16"
	c := Convert(ins, Options{ExcludeImagePatterns: []string{"postgres"}})
	if c.Classification.Verdict != "needs_review" {
		t.Fatalf("verdict = %s", c.Classification.Verdict)
	}
}

func TestUDPOnlyExcluded(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{"53/udp": {{HostIP: "127.0.0.1", HostPort: "5353"}}}
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "udp_only") {
		t.Fatalf("verdict = %s reasons=%+v", c.Classification.Verdict, c.Classification.Reasons)
	}
}

func TestAutoRemoveExcluded(t *testing.T) {
	ins := baseInspect(t)
	ins.HostConfig.AutoRemove = true
	c := Convert(ins, Options{})
	if c.Classification.Verdict != "excluded" || !hasReason(c.Classification, "auto_remove") {
		t.Fatalf("verdict = %s", c.Classification.Verdict)
	}
}

func hasReason(c model.Classification, code string) bool {
	for _, r := range c.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}
