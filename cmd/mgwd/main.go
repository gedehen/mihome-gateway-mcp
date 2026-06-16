// mgwd — Go 重写的 Mi Home Gateway Daemon
//
// 替代 daemon.mjs，通过 stdin/stdout 管理 gateway.js 子进程，
// 暴露 TCP JSON-RPC 接口 (:19345)。
//
// 用法:
//
//	mgwd --host 192.168.1.x --passcode 123456
//	mgwd --host 192.168.1.x --jsdir /path/to/mi_gateway_js
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/gedehen/mihome-gateway-mcp/internal/daemon"
)

var (
	flagHost     = flag.String("host", "", "Gateway IP (overrides MGW_HOST env)")
	flagPasscode = flag.String("passcode", "", "6-digit dynamic password (overrides MGW_PASSCODE env)")
	flagTCPAddr  = flag.String("addr", "127.0.0.1:19345", "TCP listen address")
	flagJsDir    = flag.String("jsdir", "", "daemon.mjs directory (auto-detected if empty)")
	flagNode     = flag.String("node", "", "node binary path (auto-detected if empty)")
	flagVerbose  = flag.Bool("v", false, "verbose logging")
)

func main() {
	flag.Parse()
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "mgwd — Go rewrite of Mi Home Gateway daemon\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  mgwd --host 192.168.1.x [--passcode 123456] [--addr 127.0.0.1:19345]\n\n")
		fmt.Fprintf(os.Stderr, "Config sources (host, checked in order):\n")
		fmt.Fprintf(os.Stderr, "  1. --host flag\n  2. MGW_HOST env\n  3. ~/.hermes/mihome/host file\n\n")
		fmt.Fprintf(os.Stderr, "Passcode sources:\n")
		fmt.Fprintf(os.Stderr, "  1. --passcode flag\n  2. MGW_PASSCODE env\n  3. ~/.hermes/mihome/passcode file (polled every 2s)\n  4. set_passcode TCP RPC\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// 日志
	level := slog.LevelInfo
	if *flagVerbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// 配置
	host := *flagHost
	if host == "" {
		host = os.Getenv("MGW_HOST")
	}
	passcode := *flagPasscode
	if passcode == "" {
		passcode = os.Getenv("MGW_PASSCODE")
	}

	// 自动查找 jsdir
	jsDir := *flagJsDir
	if jsDir == "" {
		jsDir = findJsDir()
	}
	if jsDir == "" {
		logger.Error("daemon.mjs directory not found — use --jsdir or place mi_gateway_js next to binary")
		os.Exit(1)
	}

	cfg := daemon.Config{
		Host:     host,
		Passcode: passcode,
		TCPAddr:  *flagTCPAddr,
		JsDir:    jsDir,
		NodeBin:  *flagNode,
	}

	d := daemon.New(cfg, logger)

	// 信号处理
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("signal received", "signal", sig)
		cancel()
	}()

	logger.Info("mgwd starting",
		"host", cfg.Host,
		"tcp", cfg.TCPAddr,
		"jsdir", cfg.JsDir,
		"passcode_set", cfg.Passcode != "",
	)

	if err := d.Run(ctx); err != nil {
		logger.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}

// findJsDir 自动查找 daemon.mjs 所在目录
func findJsDir() string {
	// 1. 可执行文件同级的 mi_gateway_js/
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Join(filepath.Dir(exe), "mi_gateway_js")
		if fileExists(filepath.Join(dir, "daemon.mjs")) {
			return dir
		}
		// 也检查上一级（开发时 go run 的情况）
		dir = filepath.Join(filepath.Dir(filepath.Dir(exe)), "mi_gateway_js")
		if fileExists(filepath.Join(dir, "daemon.mjs")) {
			return dir
		}
	}
	// 2. 当前目录
	if fileExists(filepath.Join("mi_gateway_js", "daemon.mjs")) {
		return "mi_gateway_js"
	}
	// 3. HOME
	home := os.Getenv("HOME")
	if home != "" {
		dir := filepath.Join(home, ".hermes", "mi_gateway_js")
		if fileExists(filepath.Join(dir, "daemon.mjs")) {
			return dir
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
