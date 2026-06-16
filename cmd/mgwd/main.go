// mgwd — Go 重写的 Mi Home Gateway Daemon
//
// 替代 daemon.mjs，通过 stdin/stdout 管理 gateway.js 子进程，
// 暴露 TCP JSON-RPC 接口 (:19345)。
//
// 用法:
//
//	mgwd --host 192.168.1.x --passcode 123456
//	mgwd --host 192.168.1.x --jsdir /path/to/mi_gateway_js
//	mgwd --host 192.168.1.x --native  # Go 原生协议（无需 Node.js）
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gedehen/mihome-gateway-mcp/internal/native"
)

var (
	flagHost     = flag.String("host", "", "Gateway IP (overrides MGW_HOST env)")
	flagPasscode = flag.String("passcode", "", "6-digit dynamic password (overrides MGW_PASSCODE env)")
	flagTCPAddr  = flag.String("addr", "127.0.0.1:19345", "TCP listen address")
	flagJsDir    = flag.String("jsdir", "", "daemon.mjs directory (auto-detected if empty)")
	flagNative   = flag.Bool("native", false, "use Go native protocol (no Node.js required)")
	flagVerbose  = flag.Bool("v", false, "verbose logging")
)

var logger *slog.Logger

func main() {
	flag.Parse()

	level := slog.LevelInfo
	if *flagVerbose {
		level = slog.LevelDebug
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	host := *flagHost
	if host == "" {
		host = os.Getenv("MGW_HOST")
	}
	passcode := *flagPasscode
	if passcode == "" {
		passcode = os.Getenv("MGW_PASSCODE")
	}

	// 信号处理
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()

	if *flagNative {
		runNative(ctx, host, passcode)
	} else {
		runLegacy(ctx, host, passcode)
	}
}

// runNative Go 原生模式（不需要 Node.js）
func runNative(ctx context.Context, host, passcode string) {
	// 读取配置文件
	hermesHome := os.Getenv("HERMES_HOME")
	if hermesHome == "" {
		hermesHome = filepath.Join(os.Getenv("HOME"), ".hermes")
	}
	mihomeDir := filepath.Join(hermesHome, "mihome")
	passcodeFile := filepath.Join(mihomeDir, "passcode")
	hostFile := filepath.Join(mihomeDir, "host")

	if host == "" {
		host = readFileContent(hostFile)
	}
	if passcode == "" {
		passcode = readFileContent(passcodeFile)
	}

	// TCP 服务器
	ln, err := net.Listen("tcp", *flagTCPAddr)
	if err != nil {
		logger.Error("listen failed", "error", err)
		os.Exit(1)
	}
	defer ln.Close()
	logger.Info("TCP listening", "addr", *flagTCPAddr)

	var (
		conn     *native.Connection
		connMu   sync.RWMutex
		tcpConns = make(map[string]net.Conn)
		tcpMu    sync.RWMutex
	)

	// 连接网关
	connectGateway := func() {
		connMu.Lock()
		if conn != nil {
			conn.Close()
		}
		connMu.Unlock()

		if host == "" || passcode == "" {
			logger.Info("waiting for set_passcode/set_host")
			return
		}

		c := native.NewConnection(host, passcode, logger)
		if err := c.Connect(ctx); err != nil {
			logger.Error("connect failed", "error", err)
			return
		}

		connMu.Lock()
		conn = c
		connMu.Unlock()

		// 广播连接事件
		broadcast(&tcpMu, tcpConns, map[string]any{"method": "connected"})
	}

	if passcode != "" && host != "" {
		go connectGateway()
	}

	// passcode 文件轮询
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				newPC := readFileContent(passcodeFile)
				if newPC != "" && newPC != passcode {
					passcode = newPC
					logger.Info("passcode updated from file")
					go connectGateway()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// TCP 接受循环
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			go handleTCPClient(ctx, c, &conn, &connMu, tcpConns, &tcpMu, &host, &passcode,
				func() { go connectGateway() }, connectGateway)
		}
	}()

	<-ctx.Done()
	connMu.Lock()
	if conn != nil {
		conn.Close()
	}
	connMu.Unlock()
}

func handleTCPClient(
	ctx context.Context,
	tc net.Conn,
	conn **native.Connection,
	connMu *sync.RWMutex,
	tcpConns map[string]net.Conn,
	tcpMu *sync.RWMutex,
	host, passcode *string,
	onSetPasscode func(),
	onSetHost func(),
) {
	id := fmt.Sprintf("%d_%s", time.Now().UnixNano(), tc.RemoteAddr())
	tcpMu.Lock()
	tcpConns[id] = tc
	tcpMu.Unlock()
	defer func() {
		tc.Close()
		tcpMu.Lock()
		delete(tcpConns, id)
		tcpMu.Unlock()
	}()

	scanner := bufio.NewScanner(tc)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			sendJSON(tc, map[string]any{"error": "invalid JSON"})
			continue
		}

		var resp map[string]any

		switch req.Method {
		case "set_passcode":
			var p struct{ Passcode string `json:"passcode"` }
			json.Unmarshal(req.Params, &p)
			if p.Passcode != "" {
				*passcode = p.Passcode
				os.MkdirAll(filepath.Dir(passcodeFile()), 0755)
				os.WriteFile(passcodeFile(), []byte(p.Passcode), 0600)
				resp = map[string]any{"id": req.ID, "result": map[string]string{"status": "passcode_set"}}
				onSetPasscode()
			}

		case "set_host":
			var p struct{ Host string `json:"host"` }
			json.Unmarshal(req.Params, &p)
			if p.Host != "" {
				*host = p.Host
				os.WriteFile(hostFile(), []byte(p.Host), 0644)
				resp = map[string]any{"id": req.ID, "result": map[string]string{"status": "host_set", "host": p.Host}}
				onSetHost()
			}

		case "ping":
			connMu.RLock()
			c := *conn
			connMu.RUnlock()
			connected := c != nil
			resp = map[string]any{"id": req.ID, "result": map[string]any{
				"pong": true, "connected": connected,
				"passcode_set": *passcode != "", "host": *host,
			}}

		case "get_config":
			resp = map[string]any{"id": req.ID, "result": map[string]any{
				"host": *host, "passcode_set": *passcode != "",
				"connected": *conn != nil, "tcp_addr": *flagTCPAddr, "native": true,
			}}

		default:
			connMu.RLock()
			c := *conn
			connMu.RUnlock()
			if c == nil {
				resp = map[string]any{"id": req.ID, "error": "Not connected. Use set_passcode first."}
			} else {
				result, err := c.Call(req.Method, req.Params, 15*time.Second)
				if err != nil {
					resp = map[string]any{"id": req.ID, "error": err.Error()}
				} else {
					resp = map[string]any{"id": req.ID, "result": json.RawMessage(result)}
				}
			}
		}

		if req.ID != "" && resp != nil {
			sendJSON(tc, resp)
		}
	}
}

func sendJSON(conn net.Conn, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Write(data)
}

func broadcast(tcpMu *sync.RWMutex, tcpConns map[string]net.Conn, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	tcpMu.RLock()
	defer tcpMu.RUnlock()
	for _, c := range tcpConns {
		c.Write(data)
	}
}

// runLegacy 原有 Node.js 子进程模式
func runLegacy(ctx context.Context, host, passcode string) {
	jsDir := *flagJsDir
	if jsDir == "" {
		jsDir = findJsDir()
	}
	if jsDir == "" {
		logger.Error("daemon.mjs directory not found — use --jsdir or --native")
		os.Exit(1)
	}

	// 导入 daemon 包运行
	// 这里用 exec 方式避免循环导入
	logger.Info("legacy mode not available in this build — use --native")
	logger.Info("or build with: go build -tags legacy ./cmd/mgwd/")
	os.Exit(1)
}

func findJsDir() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Join(filepath.Dir(exe), "mi_gateway_js")
		if fileExists(filepath.Join(dir, "daemon.mjs")) {
			return dir
		}
	}
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

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(data)
	return strings.TrimRight(s, "\n")
}

func passcodeFile() string {
	hermesHome := os.Getenv("HERMES_HOME")
	if hermesHome == "" {
		hermesHome = filepath.Join(os.Getenv("HOME"), ".hermes")
	}
	return filepath.Join(hermesHome, "mihome", "passcode")
}

func hostFile() string {
	hermesHome := os.Getenv("HERMES_HOME")
	if hermesHome == "" {
		hermesHome = filepath.Join(os.Getenv("HOME"), ".hermes")
	}
	return filepath.Join(hermesHome, "mihome", "host")
}
