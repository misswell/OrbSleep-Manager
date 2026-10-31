// Command orbsleepctl is the CLI for the OrbSleep daemon.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"orbsleep/internal/client"
	"orbsleep/internal/launchagent"
	"orbsleep/internal/model"
	"orbsleep/internal/settings"
)

const version = "0.2.2"

type globals struct {
	socket  string
	dataDir string
}

func main() {
	g, args := parseGlobals(os.Args[1:])
	if len(args) == 0 {
		usage()
		return
	}
	sock := g.socket
	dir := g.dataDir
	if dir == "" {
		d, err := settings.DataDir()
		if err != nil {
			fail(err)
		}
		dir = d
	}
	if sock == "" {
		sock = settings.SocketPath(dir)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("orbsleepctl " + version)
	case "help", "--help", "-h":
		usage()
	case "install-agent":
		installAgent(dir, rest)
	case "restore-all":
		withClient(sock, func(c *client.Client) { restoreAll(c) })
	case "uninstall":
		uninstall(dir, sock, rest)
	case "uninstall-agent":
		uninstallAgent()
	case "status":
		withClient(sock, func(c *client.Client) { printStatus(c, rest) })
	case "doctor":
		withClient(sock, func(c *client.Client) { printDoctor(c, rest) })
	case "recover":
		withClient(sock, func(c *client.Client) {
			var args struct {
				Transaction string `json:"transaction"`
			}
			for i := 0; i < len(rest); i++ {
				if rest[i] == "--transaction" && i+1 < len(rest) {
					args.Transaction = rest[i+1]
					i++
				}
			}
			params := map[string]any{}
			if args.Transaction != "" {
				params["transaction"] = args.Transaction
			}
			var out map[string]any
			must(c.Call(context.Background(), "system.recover", params, &out))
			fmt.Println("恢复流程已完成；请运行 orbsleepctl doctor --verify 复查。")
		})
	case "scan":
		withClient(sock, func(c *client.Client) { printScan(c, rest) })
	case "plan":
		withClient(sock, func(c *client.Client) { runPlan(c, rest) })
	case "apply":
		withClient(sock, func(c *client.Client) { runApply(c, rest) })
	case "operation", "op":
		withClient(sock, func(c *client.Client) { printOperation(c, rest) })
	case "operations":
		withClient(sock, func(c *client.Client) { printOperations(c, rest) })
	case "services":
		withClient(sock, func(c *client.Client) { printServices(c, rest) })
	case "start", "stop", "restore", "remove":
		withClient(sock, func(c *client.Client) { serviceAction(c, cmd, rest) })
	case "policy":
		withClient(sock, func(c *client.Client) { runPolicy(c, rest) })
	case "settings":
		withClient(sock, func(c *client.Client) { runSettings(c, rest) })
	case "logs":
		withClient(sock, func(c *client.Client) { printLogs(c, rest) })
	case "proxy-setup":
		withClient(sock, func(c *client.Client) { proxySetup(c) })
	case "proxy-stop":
		withClient(sock, func(c *client.Client) { proxyStop(c) })
	case "ping":
		withClient(sock, func(c *client.Client) {
			var out map[string]any
			must(c.Call(context.Background(), "ping", nil, &out))
			fmt.Printf("pong %v\n", out)
		})
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func parseGlobals(args []string) (globals, []string) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--socket" && i+1 < len(args):
			g.socket = args[i+1]
			i++
		case strings.HasPrefix(a, "--socket="):
			g.socket = strings.TrimPrefix(a, "--socket=")
		case a == "--data-dir" && i+1 < len(args):
			g.dataDir = args[i+1]
			i++
		case strings.HasPrefix(a, "--data-dir="):
			g.dataDir = strings.TrimPrefix(a, "--data-dir=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest
}

func withClient(sock string, fn func(*client.Client)) {
	c, err := client.Dial(sock)
	if err != nil {
		fail(err)
	}
	defer c.Close()
	fn(c)
}

func printStatus(c *client.Client, args []string) {
	asJSON := hasFlag(args, "--json")
	var st model.Status
	must(c.Call(context.Background(), "system.status", nil, &st))
	if asJSON {
		printJSON(st)
		return
	}
	fmt.Printf("OrbSleep daemon %s\n", st.DaemonVersion)
	fmt.Printf("Docker: %v  context=%s (%s)\n", st.DockerReachable, st.Context.Name, st.Context.Endpoint)
	proxyState := "未安装"
	if st.Proxy.Installed {
		proxyState = "已安装"
		if st.Proxy.Running {
			proxyState += "，运行中"
		} else {
			proxyState += "，未运行"
		}
	}
	fmt.Printf("代理: %s  管理接口=%v  端口范围=%d-%d  IP=%s\n", proxyState, st.Proxy.AdminReachable,
		st.Proxy.PublishedRange.Start, st.Proxy.PublishedRange.End, st.Proxy.ContainerIP)
	if st.Proxy.Error != "" {
		fmt.Printf("代理错误: %s\n", st.Proxy.Error)
	}
	fmt.Printf("托管: %d（运行 %d / 空闲 %d / 停止 %d / 缺失 %d）\n", st.Counts.Managed,
		st.Counts.Running, st.Counts.Idle, st.Counts.Stopped, st.Counts.Missing)
	fmt.Printf("扫描: 可接入 %d / 需确认 %d / 排除 %d", st.Counts.Eligible, st.Counts.NeedsReview, st.Counts.Excluded)
	if st.LastScanAt != "" {
		fmt.Printf("（最近 %s）", st.LastScanAt)
	}
	fmt.Println()
	if st.LastError.Message != "" {
		fmt.Printf("最近错误: %s (%s)\n", st.LastError.Message, st.LastError.At)
	}
}

func printDoctor(c *client.Client, args []string) {
	verify := false
	for _, a := range args {
		if a == "--verify" {
			verify = true
		}
	}
	var out struct {
		Checks []model.DoctorCheck `json:"checks"`
	}
	params := map[string]any{}
	if verify {
		params["verify"] = true
	}
	must(c.Call(context.Background(), "system.doctor", params, &out))
	for _, ch := range out.Checks {
		icon := "✔"
		switch ch.Status {
		case "warn":
			icon = "!"
		case "fail":
			icon = "✘"
		}
		fmt.Printf("%s %s: %s\n  %s\n", icon, ch.Title, ch.Status, ch.Detail)
		if ch.Remediation != "" {
			fmt.Printf("  → %s\n", ch.Remediation)
		}
	}
}

func printScan(c *client.Client, args []string) {
	asJSON := hasFlag(args, "--json")
	var res model.ScanResult
	must(c.Call(context.Background(), "scan", nil, &res))
	if asJSON {
		printJSON(res)
		return
	}
	fmt.Printf("扫描完成 %s（%d 个容器）\n\n", res.ScannedAt, len(res.Containers))
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "容器\t项目/服务\t端口\t状态\t判断\t原因")
	for _, ct := range res.Containers {
		verdict := map[string]string{"eligible": "可接入", "needs_review": "需确认", "excluded": "排除"}[ct.Classification.Verdict]
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%s\n", ct.Name, ct.ComposeProject, ct.ComposeService,
			portsShort(ct.Ports), ct.State, verdict, firstReason(ct.Classification))
	}
	w.Flush()
	fmt.Println("\n使用 `orbsleepctl plan <容器ID或名称>` 生成接入计划。")
}

func runPlan(c *client.Client, args []string) {
	allowLanFlag, applyFlag, jsonFlag := false, false, false
	var ids []string
	for _, a := range args {
		switch a {
		case "--allow-lan":
			allowLanFlag = true
		case "--apply":
			applyFlag = true
		case "--json":
			jsonFlag = true
		case "-h", "--help":
			fmt.Println("用法: orbsleepctl plan <容器ID或名称>... [--allow-lan] [--apply] [--json]")
			return
		default:
			if strings.HasPrefix(a, "-") {
				fail(fmt.Errorf("未知参数: %s", a))
			}
			ids = append(ids, a)
		}
	}
	allowLan, apply, asJSON := &allowLanFlag, &applyFlag, &jsonFlag
	if len(ids) == 0 {
		fail(fmt.Errorf("用法: orbsleepctl plan <容器ID或名称>... [--allow-lan] [--apply]"))
	}
	var plan model.Plan
	must(c.Call(context.Background(), "plan.create", map[string]any{
		"containerIds": ids, "allowLanExposure": *allowLan,
	}, &plan))
	if *asJSON {
		printJSON(plan)
	} else {
		fmt.Printf("接入计划 %s：%s\n", plan.ID, plan.Status)
		for _, b := range plan.Blockers {
			fmt.Printf("  ✘ %s: %s\n", b.Code, b.Message)
		}
		for _, it := range plan.Items {
			fmt.Printf("\n■ %s (%s/%s)\n", it.ContainerName, it.ComposeProject, it.ComposeService)
			fmt.Printf("  文件: %s\n", it.ComposeFile)
			fmt.Printf("  移除: %s\n", portsShort(it.Removals))
			fmt.Printf("  代理: %s\n", mappingsShort(it.ProxyMappings))
			fmt.Printf("  网关: %s\n", bindingsShort(it.Bindings))
			for _, r := range it.Risks {
				fmt.Printf("  · %s: %s\n", r.Code, r.Message)
			}
			if it.Diff != "" {
				fmt.Println("  --- diff ---")
				for _, line := range strings.Split(strings.TrimRight(it.Diff, "\n"), "\n") {
					fmt.Println("  " + line)
				}
			}
		}
	}
	if *apply && plan.Status == "ready" {
		runApply(c, []string{plan.ID})
	}
}

func runApply(c *client.Client, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("用法: orbsleepctl apply <planId>"))
	}
	var out struct {
		OperationID string `json:"operationId"`
	}
	must(c.Call(context.Background(), "plan.apply", map[string]any{"planId": args[0]}, &out))
	fmt.Printf("操作已开始: %s\n", out.OperationID)
	followOperation(c, out.OperationID)
}

func printOperation(c *client.Client, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("用法: orbsleepctl operation <id> [--follow]"))
	}
	follow := hasFlag(args, "--follow")
	if follow {
		followOperation(c, args[0])
		return
	}
	var op model.Operation
	must(c.Call(context.Background(), "operation.get", map[string]any{"id": args[0]}, &op))
	printJSON(op)
}

func followOperation(c *client.Client, id string) {
	last := 0
	for {
		var op model.Operation
		must(c.Call(context.Background(), "operation.get", map[string]any{"id": id}, &op))
		for ; last < len(op.Log); last++ {
			entry := op.Log[last]
			fmt.Printf("  [%s] %s %s\n", entry.Level, entry.At, entry.Message)
		}
		if op.Status != "running" {
			fmt.Printf("操作 %s: %s", op.ID, op.Status)
			if op.Error != "" {
				fmt.Printf(" — %s", op.Error)
			}
			fmt.Println()
			if op.BackupDir != "" {
				fmt.Printf("备份目录: %s\n", op.BackupDir)
			}
			if op.Status == "failed" || op.Status == "rollback_failed" {
				os.Exit(1)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func printOperations(c *client.Client, args []string) {
	limit := 50
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			limit = n
		}
	}
	var out struct {
		Operations []model.Operation `json:"operations"`
	}
	must(c.Call(context.Background(), "operations.list", map[string]any{"limit": limit}, &out))
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t类型\t状态\t开始\t结束\t错误")
	for _, op := range out.Operations {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", op.ID, op.Kind, op.Status, op.StartedAt, op.FinishedAt, op.Error)
	}
	w.Flush()
}

func printServices(c *client.Client, args []string) {
	asJSON := hasFlag(args, "--json")
	var out struct {
		Services []model.Service `json:"services"`
	}
	must(c.Call(context.Background(), "services.list", nil, &out))
	if asJSON {
		printJSON(out.Services)
		return
	}
	if len(out.Services) == 0 {
		fmt.Println("暂无托管服务。")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "服务ID\t容器\t状态\t连接\t原端口\t代理端口\t空闲\t最后活动")
	for _, svc := range out.Services {
		var orig, proxyPorts []string
		for _, b := range svc.Bindings {
			orig = append(orig, fmt.Sprintf("%d", b.HostPort))
			proxyPorts = append(proxyPorts, fmt.Sprintf("%d", b.ProxyPort))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%ds\t%s\n", svc.ID, svc.ContainerName, svc.Status,
			svc.ActiveConnections, strings.Join(orig, ","), strings.Join(proxyPorts, ","), svc.IdleTimeoutSecs, svc.LastActiveAt)
	}
	w.Flush()
}

func serviceAction(c *client.Client, action string, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("用法: orbsleepctl %s <serviceId>", action))
	}
	var out map[string]any
	must(c.Call(context.Background(), "service.action", map[string]any{
		"id": args[0], "action": action, "params": map[string]any{},
	}, &out))
	if id, ok := out["operationId"].(string); ok && id != "" {
		fmt.Printf("操作已开始: %s\n", id)
		followOperation(c, id)
		return
	}
	printJSON(out)
}

func runPolicy(c *client.Client, args []string) {
	idle := 0
	exclude := false
	var ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--idle" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				fail(fmt.Errorf("--idle 需要整数"))
			}
			idle = n
			i++
		case strings.HasPrefix(a, "--idle="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--idle="))
			if err != nil {
				fail(fmt.Errorf("--idle 需要整数"))
			}
			idle = n
		case a == "--exclude-sleep":
			exclude = true
		default:
			if strings.HasPrefix(a, "-") {
				fail(fmt.Errorf("未知参数: %s", a))
			}
			ids = append(ids, a)
		}
	}
	if len(ids) == 0 {
		fail(fmt.Errorf("用法: orbsleepctl policy <serviceId> [--idle 1800] [--exclude-sleep]"))
	}
	params := map[string]any{}
	if idle > 0 {
		params["idleTimeoutSecs"] = idle
	}
	if exclude {
		params["excludedFromSleep"] = true
	}
	var out map[string]any
	must(c.Call(context.Background(), "service.action", map[string]any{
		"id": ids[0], "action": "updatePolicy", "params": params,
	}, &out))
	printJSON(out)
}

func runSettings(c *client.Client, args []string) {
	if len(args) > 0 && args[0] == "set" {
		patch := map[string]any{}
		for _, kv := range args[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				fail(fmt.Errorf("用法: settings set key=value（%s 无效）", kv))
			}
			key, val := parts[0], parts[1]
			switch key {
			case "idleTimeoutSecs", "startTimeoutSecs", "proxyPortStart", "proxyPortEnd", "adminPort", "webPort", "logRetentionDays":
				n, err := strconv.Atoi(val)
				if err != nil {
					fail(fmt.Errorf("%s 需要整数", key))
				}
				patch[key] = n
			case "autoScanOnLaunch", "enableLowPortHelper":
				b, err := strconv.ParseBool(val)
				if err != nil {
					fail(fmt.Errorf("%s 需要 true/false", key))
				}
				patch[key] = b
			case "backupDir", "proxyDialMode":
				patch[key] = val
			case "excludeImagePatterns":
				patch[key] = strings.Split(val, ",")
			default:
				fail(fmt.Errorf("未知设置项: %s", key))
			}
		}
		var s model.Settings
		must(c.Call(context.Background(), "settings.update", patch, &s))
		printJSON(s)
		return
	}
	var s model.Settings
	must(c.Call(context.Background(), "settings.get", nil, &s))
	printJSON(s)
}

func printLogs(c *client.Client, args []string) {
	limit := 100
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				limit = n
			}
			i++
		}
	}
	var out struct {
		Lines []model.LogEntry `json:"lines"`
	}
	must(c.Call(context.Background(), "logs.tail", map[string]any{"limit": limit}, &out))
	for _, line := range out.Lines {
		fmt.Printf("%s %-5s %s\n", line.At, line.Level, line.Message)
	}
}

func proxyStop(c *client.Client) {
	var out struct {
		OperationID string `json:"operationId"`
	}
	must(c.Call(context.Background(), "proxy.stop", nil, &out))
	fmt.Printf("停止代理操作: %s\n", out.OperationID)
	followOperation(c, out.OperationID)
}

func proxySetup(c *client.Client) {
	var out struct {
		OperationID string `json:"operationId"`
	}
	must(c.Call(context.Background(), "proxy.setup", nil, &out))
	fmt.Printf("代理安装/启动操作: %s\n", out.OperationID)
	followOperation(c, out.OperationID)
}

func installAgent(dataDir string, args []string) {
	fs := flag.NewFlagSet("install-agent", flag.ContinueOnError)
	binary := fs.String("binary", "", "orbsleepd 可执行文件路径")
	fs.Parse(args)
	bin := *binary
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			fail(err)
		}
		bin = filepath.Join(filepath.Dir(self), "orbsleepd")
	}
	if _, err := os.Stat(bin); err != nil {
		fail(fmt.Errorf("找不到 orbsleepd: %s（请先 go build，或用 --binary 指定）", bin))
	}
	if err := launchagent.Install(bin, dataDir); err != nil {
		fail(err)
	}
	fmt.Printf("已安装 LaunchAgent: %s\n", launchagent.Label)
	for i := 0; i < 40; i++ {
		if c, err := client.Dial(settings.SocketPath(dataDir)); err == nil {
			c.Close()
			fmt.Println("后台助手已启动。")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println("LaunchAgent 已安装，但后台助手暂未响应；查看 ~/Library/Logs 或 logs/launchd.err.log")
}

func restoreAll(c *client.Client) {
	var out struct {
		Services []model.Service `json:"services"`
	}
	must(c.Call(context.Background(), "services.list", nil, &out))
	if len(out.Services) == 0 {
		fmt.Println("没有需要恢复的托管服务。")
		return
	}
	for _, svc := range out.Services {
		fmt.Printf("恢复 %s（%s）…\n", svc.ContainerName, svc.ID)
		serviceAction(c, "restore", []string{svc.ID})
	}
	fmt.Printf("已恢复 %d 个服务。\n", len(out.Services))
}

func uninstall(dataDir, sock string, args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "确认执行（必须显式指定）")
	keepProxy := fs.Bool("keep-proxy", false, "保留代理容器与数据目录")
	force := fs.Bool("force", false, "后台助手无法启动时仍然卸载（可能导致服务中断）")
	fs.Parse(args)
	if !*yes {
		fail(fmt.Errorf("卸载会停止并恢复所有托管服务。确认请加 --yes"))
	}
	if *force {
		path, err := exportRecoveryArchive(dataDir)
		if err != nil {
			fail(fmt.Errorf("导出恢复材料失败，已中止 --force 卸载: %w", err))
		}
		fmt.Fprintln(os.Stderr, "警告：--force 可能中断仍托管服务；恢复材料已导出到", path)
	}
	c := connectDaemonForUninstall(sock, *force)
	if c != nil {
		restoreAll(c)
		var out struct {
			Services []model.Service `json:"services"`
		}
		must(c.Call(context.Background(), "services.list", nil, &out))
		c.Close()
		if len(out.Services) > 0 {
			if !*force {
				path, _ := exportRecoveryArchive(dataDir)
				fail(fmt.Errorf("仍有 %d 个托管服务未恢复，已中止卸载；恢复材料: %s", len(out.Services), path))
			}
			fmt.Fprintf(os.Stderr, "警告：仍有 %d 个托管服务未恢复，--force 继续卸载。\n", len(out.Services))
		}
	} else if !*force {
		fail(fmt.Errorf("后台助手不可用，无法确认托管服务已恢复；请先启动 orbsleepd，或使用 --force"))
	}

	if err := launchagent.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "移除 LaunchAgent 失败:", err)
	} else {
		fmt.Println("已移除 LaunchAgent。")
	}
	if !*keepProxy {
		composeFile := filepath.Join(dataDir, "proxy", "docker-compose.yml")
		if _, err := os.Stat(composeFile); err == nil {
			docker, err := findDockerLocal()
			if err != nil {
				fmt.Fprintln(os.Stderr, "未找到 docker CLI，代理容器未移除:", err)
			} else if out, err := runLocal(docker, "compose", "-p", "orbsleep-proxy", "-f", composeFile, "down"); err != nil {
				fmt.Fprintln(os.Stderr, "停止代理容器失败:", err, out)
			} else {
				fmt.Println("已停止代理容器（配置与备份保留）。")
			}
		}
	}
	fmt.Printf("OrbSleep 数据（数据库、日志、备份）保留在: %s\n", dataDir)
	fmt.Println("如确认不再需要，可在备份后手动删除该目录。")
}

func exportRecoveryArchive(dataDir string) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(dataDir, "recovery-export-"+stamp+".tar.gz")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(dataDir, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), "recovery-export-") || strings.HasSuffix(p, ".sock") {
			return nil
		}
		rel, rerr := filepath.Rel(dataDir, p)
		if rerr != nil {
			return rerr
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		rf, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		_, cerr := io.Copy(tw, rf)
		_ = rf.Close()
		return cerr
	})
	if err != nil {
		tw.Close()
		gz.Close()
		os.Remove(path)
		return "", err
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return path, f.Close()
}

func connectDaemonForUninstall(sock string, force bool) *client.Client {
	if c, err := client.Dial(sock); err == nil {
		return c
	}
	if launchagent.Installed() {
		if err := launchagent.Restart(); err != nil {
			fmt.Fprintln(os.Stderr, "尝试启动后台助手失败:", err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if c, err := client.Dial(sock); err == nil {
			return c
		}
	}
	if !force {
		return nil
	}
	fmt.Println("警告：后台助手仍不可用，--force 继续卸载；请自行确认没有仍需恢复的服务。")
	return nil
}

func findDockerLocal() (string, error) {
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		"/Applications/OrbStack.app/Contents/MacOS/bin/docker",
		"/usr/local/bin/docker",
		"/opt/homebrew/bin/docker",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 docker CLI")
}

func runLocal(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func uninstallAgent() {
	if err := launchagent.Uninstall(); err != nil {
		fail(err)
	}
	fmt.Println("已卸载 LaunchAgent。")
}

func portsShort(list []model.PortBinding) string {
	var parts []string
	for _, p := range list {
		host := p.HostIP
		if host == "" {
			host = "0.0.0.0"
		}
		parts = append(parts, fmt.Sprintf("%s:%d→%d/%s", host, p.HostPort, p.ContainerPort, p.Protocol))
	}
	return strings.Join(parts, ", ")
}

func mappingsShort(list []model.ProxyMapping) string {
	var parts []string
	for _, m := range list {
		parts = append(parts, fmt.Sprintf("%d→%d", m.ListenPort, m.TargetPort))
	}
	return strings.Join(parts, ", ")
}

func bindingsShort(list []model.Binding) string {
	var parts []string
	for _, b := range list {
		host := b.HostIP
		if host == "" {
			host = "0.0.0.0"
		}
		parts = append(parts, fmt.Sprintf("%s:%d→%d", host, b.HostPort, b.ProxyPort))
	}
	return strings.Join(parts, ", ")
}

func firstReason(c model.Classification) string {
	var reasons []string
	for _, r := range c.Reasons {
		if r.Severity == "blocker" {
			reasons = append(reasons, r.Message)
		}
	}
	if len(reasons) == 0 {
		for _, r := range c.Warnings {
			reasons = append(reasons, r.Message)
		}
	}
	if len(reasons) == 0 {
		for _, r := range c.Reasons {
			reasons = append(reasons, r.Message)
		}
	}
	if len(reasons) > 2 {
		reasons = reasons[:2]
	}
	sort.Strings(reasons)
	return strings.Join(reasons, "；")
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

func usage() {
	fmt.Print(`orbsleepctl — OrbSleep 管理 CLI

用法: orbsleepctl [--socket PATH] [--data-dir DIR] <命令> [参数]

命令:
  status [--json]                 概览状态
  doctor [--verify]               安装与连通性检查；--verify 做深度一致性检查
  recover [--transaction ID]       恢复未完成事务/指定事务
  scan [--json]                   扫描全部容器
  plan <id|name>... [--allow-lan] [--apply]
                                  生成接入计划
  apply <planId>                  执行计划并跟踪进度
  operation <id> [--follow]       查看操作
  operations [limit]              操作历史
  services [--json]               托管服务
  start|stop|restore|remove <id>  生命周期操作
  policy <id> [--idle N] [--exclude-sleep]
                                  修改休眠策略
  settings [set key=value...]     查看/修改设置
  logs [-n 100]                   守护进程日志
  proxy-setup                     安装/启动 Lazy TCP Proxy
  proxy-stop                      停止/移除代理容器（保留配置）
  restore-all                     恢复所有托管服务
  uninstall --yes [--keep-proxy]  卸载（恢复服务、移除开机启动）
  install-agent [--binary PATH]   安装开机启动
  uninstall-agent                 移除开机启动
  ping                            检查 daemon
  version                         版本
`)
}
