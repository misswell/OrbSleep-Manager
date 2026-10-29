package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbsleep/internal/launchagent"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

// Doctor runs installation and connectivity checks.
func (e *Engine) Doctor() []model.DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var checks []model.DoctorCheck
	add := func(id, title, status, detail, remediation string) {
		checks = append(checks, model.DoctorCheck{ID: id, Title: title, Status: status, Detail: detail, Remediation: remediation})
	}
	s := e.settings.Get()

	c, err := e.dockerClient(ctx)
	if err != nil {
		add("docker", "Docker / OrbStack 连接", "fail", err.Error(),
			"启动 OrbStack（或 Docker Desktop）后重试；本工具不会切换全局 Docker Context")
	} else {
		add("docker", "Docker / OrbStack 连接", "ok",
			fmt.Sprintf("%s · Docker %s · %s", c.Context().Name, c.Version(), c.Socket()), "")
		kind := c.Context().Kind
		if kind == "orbstack" {
			add("context", "OrbStack Context", "ok", "已识别 orbstack context（未修改全局默认 context）", "")
		} else {
			add("context", "OrbStack Context", "warn", "当前连接的不是 OrbStack context: "+c.Context().Name,
				"如果使用 OrbStack，请启动 OrbStack；OrbSleep 只读取当前 context，不会替你切换")
		}
		if cli, err := e.cliRunner(); err != nil {
			add("compose", "Docker Compose CLI", "fail", err.Error(), "OrbStack 自带 docker compose，请确认 docker CLI 可用")
		} else if v, err := cli.Version(ctx); err != nil {
			add("compose", "Docker Compose CLI", "warn", err.Error(), "")
		} else {
			add("compose", "Docker Compose CLI", "ok", "docker "+v, "")
		}
	}

	if _, err := os.Stat(e.proxyComposePath()); err != nil {
		add("proxy_files", "代理配置文件", "fail", "尚未安装："+e.proxyComposePath(),
			"在「设置」中点击安装/启动代理")
	} else {
		add("proxy_files", "代理配置文件", "ok", e.proxyComposePath(), "")
	}

	ps := e.proxyStatus(ctx)
	switch {
	case !ps.Installed:
		add("proxy_container", "Lazy TCP Proxy 容器", "fail", "未安装", "在「设置」中安装代理")
	case !ps.Running:
		add("proxy_container", "Lazy TCP Proxy 容器", "fail", "容器已创建但未运行: "+ps.Error, "启动代理容器")
	case !ps.AdminReachable:
		add("proxy_container", "Lazy TCP Proxy 容器", "fail", "管理接口不可达: "+ps.Error,
			fmt.Sprintf("检查 127.0.0.1:%d 是否有其他程序占用，或重新安装代理", s.AdminPort))
	default:
		add("proxy_container", "Lazy TCP Proxy 容器", "ok",
			fmt.Sprintf("运行中 · 内部 IP %s · 管理端口 %d · 发布端口 %d-%d", ps.ContainerIP, s.AdminPort, s.ProxyPortStart, s.ProxyPortEnd), "")
	}
	if ps.Running {
		addr := fmt.Sprintf("127.0.0.1:%d", s.ProxyPortStart)
		conn, err := netDialer(2*time.Second).DialContext(ctx, "tcp", addr)
		if err != nil {
			add("proxy_reachable", "代理监听端口可访问", "fail", fmt.Sprintf("macOS 无法连接 %s: %v", addr, err),
				fmt.Sprintf("确认代理容器发布端口范围 %d-%d；该检查对应阶段 0 的关键网络验证", s.ProxyPortStart, s.ProxyPortEnd))
		} else {
			conn.Close()
			add("proxy_reachable", "代理监听端口可访问", "ok", "macOS 可直接连接 "+addr+"（容器 IP 访问不是必需）", "")
		}
	}

	// Data directories.
	if err := writable(e.dataDir); err != nil {
		add("data_dir", "数据目录可写", "fail", err.Error(), "检查磁盘权限")
	} else {
		add("data_dir", "数据目录可写", "ok", e.dataDir, "")
	}
	if err := writable(s.BackupDir); err != nil {
		add("backup_dir", "备份目录可写", "fail", err.Error(), "在设置中更换备份目录")
	} else {
		add("backup_dir", "备份目录可写", "ok", s.BackupDir, "")
	}

	if launchagent.Installed() {
		add("launch_agent", "后台助手开机启动", "ok", "LaunchAgent 已安装（"+launchagent.Label+"）", "")
	} else {
		add("launch_agent", "后台助手开机启动", "warn", "尚未安装 LaunchAgent；关闭 GUI 后端口网关会随守护进程退出",
			"运行 orbsleepctl install-agent，或在设置中启用")
	}

	if s.EnableLowPortHelper {
		add("low_ports", "特权端口助手", "warn", "低端口支持尚未实现，低于 1024 的端口仍会被排除", "第一版请使用 >=1024 的宿主机端口")
	} else {
		add("low_ports", "低端口支持", "ok", "低于 1024 的端口会被标记为不支持（符合当前设计）", "")
	}

	if c != nil {
		if err := e.checkForeignProxies(ctx); err != nil {
			add("foreign_proxy", "其他 Lazy TCP Proxy", "warn", err.Error(), "OrbSleep 不会删除或覆盖它；如需复用请手动配置")
		} else {
			add("foreign_proxy", "其他 Lazy TCP Proxy", "ok", "未发现同名冲突或外部代理实例", "")
		}
	}
	return checks
}

func (e *Engine) checkForeignProxies(ctx context.Context) error {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil
	}
	list, err := c.ListContainers(ctx)
	if err != nil {
		return nil
	}
	var foreign []string
	for _, item := range list {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		if name == proxy.ContainerName {
			continue
		}
		if strings.Contains(item.Image, "lazy-tcp-proxy") {
			foreign = append(foreign, name+" ("+item.Image+")")
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("发现 %s；OrbSleep 使用独立的 %s，不会复用或删除已有实例", strings.Join(foreign, ", "), proxy.ContainerName)
	}
	return nil
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".orbsleep-write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// CleanupOldOperations prunes operation history.
func (e *Engine) CleanupOldOperations() {
	days := e.settings.Get().LogRetentionDays
	if days <= 0 {
		days = 14
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	if err := e.store.PruneOperations(cutoff); err != nil {
		e.logger.Printf("warn", "清理操作历史失败: %v", err)
	}
	if err := e.logger.CleanupOlderThan(days); err != nil {
		e.logger.Printf("warn", "清理日志文件失败: %v", err)
	}
	e.cleanupOldBackups(cutoff)
}

// cleanupOldBackups removes old operation backup directories but never touches
// the per-service recovery backups under backups/services.
func (e *Engine) cleanupOldBackups(cutoff time.Time) {
	root := e.settings.Get().BackupDir
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "services" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			e.logger.Printf("warn", "清理旧备份失败 %s: %v", entry.Name(), err)
		}
	}
}

func (e *Engine) retentionLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.CleanupOldOperations()
		}
	}
}
