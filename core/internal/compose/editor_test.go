package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbsleep/internal/model"
)

const fixture = `# top comment
name: demo
services:
  web:
    image: nginx:alpine
    # keep this comment
    ports:
      - "127.0.0.1:18080:80"
      - "127.0.0.1:18081:81/tcp"
      - "19090:9090"
    volumes:
      - ./html:/usr/share/nginx/html:ro
  api:
    image: alpine
    ports:
      - "127.0.0.1:18082:82"
`

func TestRemoveSinglePortKeepsEverythingElse(t *testing.T) {
	remove := []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"}}
	out, removed, err := RemoveBindings([]byte(fixture), "web", remove)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %d", len(removed))
	}
	text := string(out)
	if strings.Contains(text, "18080:80") {
		t.Fatalf("port still present:\n%s", text)
	}
	for _, want := range []string{"18081:81/tcp", "19090:9090", "# keep this comment", "api:", "18082:82"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	// Re-parse to be sure the file remains valid.
	if _, err := ReadDecls(out, "web"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
}

func TestRemoveAllPortsDropsKey(t *testing.T) {
	remove := []model.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 18081, ContainerPort: 81, Protocol: "tcp"},
		{HostIP: "", HostPort: 19090, ContainerPort: 9090, Protocol: "tcp"},
	}
	out, removed, err := RemoveBindings([]byte(fixture), "web", remove)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %d", len(removed))
	}
	text := string(out)
	if strings.Contains(text, "ports:") && strings.Contains(text, "web:") {
		// "api" still has ports, so only ensure the web block lost its ports key.
		block := text[strings.Index(text, "web:"):strings.Index(text, "api:")]
		if strings.Contains(block, "ports:") {
			t.Fatalf("web still has ports key:\n%s", text)
		}
	}
	if !strings.Contains(text, "18082:82") {
		t.Fatalf("api port lost:\n%s", text)
	}
}

func TestRemoveMismatchFails(t *testing.T) {
	remove := []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 9999, ContainerPort: 80, Protocol: "tcp"}}
	if _, _, err := RemoveBindings([]byte(fixture), "web", remove); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestLongSyntax(t *testing.T) {
	src := `services:
  api:
    image: app
    ports:
      - target: 8080
        published: "18000"
        host_ip: 127.0.0.1
        protocol: tcp
      - target: 9090
        published: "19000"
`
	decls, err := ReadDecls([]byte(src), "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 2 {
		t.Fatalf("decls = %d", len(decls))
	}
	out, removed, err := RemoveBindings([]byte(src), "api", []model.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 18000, ContainerPort: 8080, Protocol: "tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %d", len(removed))
	}
	if !strings.Contains(string(out), "9090") || strings.Contains(string(out), "18000") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestFlowStyleRejected(t *testing.T) {
	src := "services:\n  api:\n    image: app\n    ports: [\"8080:80\"]\n"
	if _, err := ReadDecls([]byte(src), "api"); err == nil {
		t.Fatal("expected flow style to be rejected")
	}
}

func TestParseShort(t *testing.T) {
	cases := []struct {
		in   string
		want model.PortBinding
		err  bool
	}{
		{"8080:80", model.PortBinding{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
		{"127.0.0.1:8080:80", model.PortBinding{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
		{"0.0.0.0:8080:80", model.PortBinding{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
		{"8080:80/udp", model.PortBinding{HostPort: 8080, ContainerPort: 80, Protocol: "udp"}, false},
		{"[::1]:8080:80", model.PortBinding{HostIP: "::1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
		{"8000-8010:80", model.PortBinding{}, true},
		{"abc:80", model.PortBinding{}, true},
	}
	for _, c := range cases {
		got, err := ParseShort(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestValidateEffective(t *testing.T) {
	before := map[string]any{
		"services": map[string]any{
			"web": map[string]any{
				"image": "nginx",
				"ports": []any{
					map[string]any{"mode": "ingress", "target": 80, "published": "18080", "protocol": "tcp", "host_ip": "127.0.0.1"},
					map[string]any{"mode": "ingress", "target": 90, "published": "18090", "protocol": "tcp"},
				},
			},
		},
	}
	after := map[string]any{
		"services": map[string]any{
			"web": map[string]any{
				"image": "nginx",
				"ports": []any{
					map[string]any{"mode": "ingress", "target": 90, "published": "18090", "protocol": "tcp"},
				},
			},
		},
	}
	remove := []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"}}
	if err := ValidateEffective(before, after, "web", remove); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
	after["services"].(map[string]any)["web"].(map[string]any)["image"] = "other"
	if err := ValidateEffective(before, after, "web", remove); err == nil {
		t.Fatal("expected unrelated change to be detected")
	}
}

func TestDiffForRemovals(t *testing.T) {
	all, err := ReadDecls([]byte(fixture), "web")
	if err != nil {
		t.Fatal(err)
	}
	var decls []PortDecl
	for _, d := range all {
		if d.Binding.HostPort == 18080 {
			decls = append(decls, d)
		}
	}
	diff := DiffForRemovals(fixture, decls, "docker-compose.yml")
	if !strings.Contains(diff, "-      - \"127.0.0.1:18080:80\"") {
		t.Fatalf("diff missing removed line:\n%s", diff)
	}
	if !strings.Contains(diff, "+++ b/docker-compose.yml") {
		t.Fatalf("diff header missing:\n%s", diff)
	}
}

func TestAddBindingsRoundTrip(t *testing.T) {
	remove := []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"}}
	without, _, err := RemoveBindings([]byte(fixture), "web", remove)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := AddBindings(without, "web", remove); err != nil {
		t.Fatalf("add: %v", err)
	}
	restored, _, err := AddBindings(without, "web", remove)
	if err != nil {
		t.Fatal(err)
	}
	// The output must be valid, contain the port again, and keep other ports.
	decls, err := ReadDecls(restored, "web")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range decls {
		if d.Binding.HostPort == 18080 && d.Binding.ContainerPort == 80 {
			found = true
		}
	}
	if !found {
		t.Fatalf("binding not restored:\n%s", restored)
	}
	if !strings.Contains(string(restored), "18082:82") || !strings.Contains(string(restored), "# keep this comment") {
		t.Fatalf("other content lost:\n%s", restored)
	}
	// Re-adding is idempotent.
	again, added, err := AddBindings(restored, "web", remove)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 || string(again) != string(restored) {
		t.Fatalf("not idempotent: added=%d", len(added))
	}
}

func TestAddBindingsWhenPortsKeyMissing(t *testing.T) {
	src := "services:\n  api:\n    image: app\n    environment:\n      FOO: bar\n  other:\n    image: x\n"
	out, added, err := AddBindings([]byte(src), "api", []model.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 18000, ContainerPort: 8080, Protocol: "tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("added = %d", len(added))
	}
	if !strings.Contains(string(out), "    ports:\n      - \"127.0.0.1:18000:8080\"") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	decls, err := ReadDecls(out, "api")
	if err != nil || len(decls) != 1 {
		t.Fatalf("decls=%v err=%v", decls, err)
	}
}

func TestAddBindingsLongSyntax(t *testing.T) {
	src := `services:
  api:
    image: app
    ports:
      - target: 9090
        published: "19000"
        host_ip: 127.0.0.1
        protocol: tcp
`
	out, added, err := AddBindings([]byte(src), "api", []model.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 18000, ContainerPort: 8080, Protocol: "tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("added=%d\n%s", len(added), out)
	}
	decls, err := ReadDecls(out, "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 2 {
		t.Fatalf("decls=%d\n%s", len(decls), out)
	}
	if !strings.Contains(string(out), "18000") {
		t.Fatalf("new binding missing:\n%s", out)
	}
}

func TestClearServiceTCPPortsKeepsUDP(t *testing.T) {
	content := []byte(`services:
  web:
    image: alpine
    ports:
      - "18080:80"
      - "18081:81/tcp"
      - "19090:9090/udp"
  api:
    image: alpine
    ports:
      - "18082:82"
`)
	out, err := ClearServiceTCPPorts(content, "web")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "18080") || strings.Contains(string(out), "18081") {
		t.Fatalf("TCP ports were not cleared:\n%s", out)
	}
	if !strings.Contains(string(out), "19090:9090/udp") {
		t.Fatalf("UDP port must be preserved:\n%s", out)
	}
	if !strings.Contains(string(out), "api:") || !strings.Contains(string(out), "18082") {
		t.Fatalf("other services must be untouched:\n%s", out)
	}

	// A service without ports, or one that does not exist at all, is a no-op.
	out, err = ClearServiceTCPPorts(out, "web")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := ClearServiceTCPPorts(content, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(content) {
		t.Fatalf("missing service must be a no-op:\n%s", unchanged)
	}

	flow := []byte("services:\n  api: {image: alpine, ports: [\"18082:82\"]}\n")
	if _, err := ClearServiceTCPPorts(flow, "api"); err == nil {
		t.Fatal("flow-style ports must be rejected")
	}
}

func TestWriteAtomicPreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-compose.yml")
	link := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(link, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("WriteAtomic replaced the symlink with a regular file")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "new\n" {
		t.Fatalf("target content = %q", raw)
	}
}
