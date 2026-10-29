// Package dockercli invokes the Docker CLI and the Compose v2 plugin against an
// explicit DOCKER_HOST, so OrbSleep never depends on (or changes) the user's
// global Docker context.
package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner executes docker commands.
type Runner struct {
	Bin  string
	Host string // unix:///...
}

// Find locates a docker CLI and binds it to the given daemon host.
func Find(host string) (*Runner, error) {
	candidates := []string{}
	if p, err := exec.LookPath("docker"); err == nil {
		candidates = append(candidates, p)
	}
	home, _ := os.UserHomeDir()
	candidates = append(candidates,
		"/Applications/OrbStack.app/Contents/MacOS/bin/docker",
		filepath.Join(home, ".orbstack", "bin", "docker"),
		"/usr/local/bin/docker",
		"/opt/homebrew/bin/docker",
	)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return &Runner{Bin: p, Host: host}, nil
		}
	}
	return nil, fmt.Errorf("未找到 docker CLI（OrbStack 通常自带）")
}

func (r *Runner) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DOCKER_HOST=") || strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "DOCKER_HOST="+r.Host)
	return out
}

func (r *Runner) run(ctx context.Context, dir string, timeout time.Duration, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Bin, args...)
	cmd.Env = r.env()
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("docker 命令超时: docker %s", strings.Join(args, " "))
		}
		return nil, fmt.Errorf("docker %s 失败: %v: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// Compose runs a compose subcommand in the project directory.
func (r *Runner) Compose(ctx context.Context, project, file, workdir string, timeout time.Duration, args ...string) ([]byte, error) {
	full := []string{"compose"}
	if project != "" {
		full = append(full, "-p", project)
	}
	if workdir != "" {
		full = append(full, "--project-directory", workdir)
	}
	if file != "" {
		full = append(full, "-f", file)
	}
	full = append(full, args...)
	return r.run(ctx, workdir, timeout, full...)
}

// ComposeConfig returns the effective merged configuration.
func (r *Runner) ComposeConfig(ctx context.Context, project, file, workdir string) (map[string]any, error) {
	out, err := r.Compose(ctx, project, file, workdir, 60*time.Second, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("解析 compose config 输出失败: %w", err)
	}
	return cfg, nil
}

// UpService recreates only the given service.
func (r *Runner) UpService(ctx context.Context, project, file, workdir, service string) error {
	_, err := r.Compose(ctx, project, file, workdir, 5*time.Minute, "up", "-d", "--no-deps", "--force-recreate", service)
	return err
}

// ProxyUp starts (or recreates) the OrbSleep proxy compose project.
func (r *Runner) ProxyUp(ctx context.Context, file string) error {
	_, err := r.run(ctx, filepath.Dir(file), 5*time.Minute,
		"compose", "-p", "orbsleep-proxy", "-f", file, "up", "-d", "--remove-orphans")
	return err
}

// ProxyDown stops the OrbSleep proxy project.
func (r *Runner) ProxyDown(ctx context.Context, file string) error {
	_, err := r.run(ctx, filepath.Dir(file), 2*time.Minute, "compose", "-p", "orbsleep-proxy", "-f", file, "down")
	return err
}

// Version returns the docker client version.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "", 15*time.Second, "version", "--format", "{{.Client.Version}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
