// Command orbsleepd is the per-user OrbSleep background daemon. It owns the
// host port listeners, talks to Docker, and drives the lazy TCP proxy.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"orbsleep/internal/api"
	"orbsleep/internal/engine"
	"orbsleep/internal/settings"
)

const version = "0.1.8"

func main() {
	// Keep state, logs and backups readable by the current user only.
	syscall.Umask(0o077)
	var (
		dataDir = flag.String("data-dir", "", "应用数据目录（默认 ~/Library/Application Support/OrbSleep）")
		socket  = flag.String("socket", "", "Unix socket 路径（默认 <data-dir>/daemon.sock）")
		showVer = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	dir := *dataDir
	if dir == "" {
		d, err := settings.DataDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "无法确定数据目录:", err)
			os.Exit(1)
		}
		dir = d
	}
	sock := *socket
	if sock == "" {
		sock = settings.SocketPath(dir)
	}
	eng, err := engine.New(version, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败:", err)
		os.Exit(1)
	}
	srv, err := api.New(sock, eng)
	if err != nil {
		eng.Close()
		if errors.Is(err, api.ErrAlreadyRunning) {
			// Exit successfully so launchd (KeepAlive.SuccessfulExit=false)
			// does not spin restarting a duplicate daemon.
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Fprintln(os.Stderr, "启动本地接口失败:", err)
		os.Exit(1)
	}
	eng.CleanupOldOperations()
	eng.Start()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		srv.Close()
		eng.Close()
		os.Exit(0)
	}()

	fmt.Printf("orbsleepd %s 已启动\n  socket: %s\n  数据目录: %s\n", version, sock, dir)
	if err := srv.Serve(); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
		srv.Close()
		eng.Close()
		os.Exit(1)
	}
}
