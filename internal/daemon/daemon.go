// Package daemon 实现 TCP JSON-RPC 服务器，管理 gateway.js 子进程
//
// 架构：
//
//	任意客户端 → TCP :19345 → Go daemon → stdin/stdout JSON-RPC → daemon.mjs → gateway.js → 网关
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gedehen/mihome-gateway-mcp/internal/gateway"
)

// RPCRequest JSON-RPC 请求
type RPCRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCResponse JSON-RPC 响应
type RPCResponse struct {
	ID     string      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// Config daemon 配置
type Config struct {
	Host     string
	Passcode string
	TCPAddr  string // 默认 "127.0.0.1:19345"
	JsDir    string // daemon.mjs 目录
	NodeBin  string

	HermesHome string // 默认 ~/.hermes
}

// Daemon 核心
type Daemon struct {
	cfg    Config
	logger *slog.Logger
	gw     *gateway.Manager

	mu           sync.RWMutex
	connected    bool
	passcode     string
	host         string
	tcpClients   map[string]net.Conn
	reconnectN   int
	shuttingDown bool
	gwEvents     <-chan gateway.Event

	// 路径
	mihomeDir    string
	passcodeFile string
	hostFile     string
}

const (
	reconnectMin = 3 * time.Second
	reconnectMax = 30 * time.Second
	keepaliveInt = 25 * time.Second
	passcodePoll = 2 * time.Second
	maxReconnect = 5
	rpcTimeout   = 15 * time.Second
)

// New 创建 Daemon
func New(cfg Config, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.TCPAddr == "" {
		cfg.TCPAddr = "127.0.0.1:19345"
	}
	if cfg.HermesHome == "" {
		cfg.HermesHome = filepath.Join(os.Getenv("HOME"), ".hermes")
	}
	mihomeDir := filepath.Join(cfg.HermesHome, "mihome")

	d := &Daemon{
		cfg:          cfg,
		logger:       logger,
		passcode:     cfg.Passcode,
		host:         cfg.Host,
		tcpClients:   make(map[string]net.Conn),
		mihomeDir:    mihomeDir,
		passcodeFile: filepath.Join(mihomeDir, "passcode"),
		hostFile:     filepath.Join(mihomeDir, "host"),
	}

	if d.host == "" {
		d.host = readFile(d.hostFile)
	}
	if d.passcode == "" {
		d.passcode = readFile(d.passcodeFile)
	}
	return d
}

// Run 启动 daemon（阻塞直到 ctx 取消）
func (d *Daemon) Run(ctx context.Context) error {
	os.MkdirAll(d.mihomeDir, 0755)

	ln, err := net.Listen("tcp", d.cfg.TCPAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.cfg.TCPAddr, err)
	}
	defer ln.Close()
	d.logger.Info("TCP listening", "addr", d.cfg.TCPAddr)

	go d.acceptLoop(ctx, ln)
	go d.passcodePoller(ctx)

	if d.passcode != "" && d.host != "" {
		d.logger.Info("passcode available, connecting...")
		d.connectGateway(ctx)
	} else {
		d.logger.Info("waiting for set_passcode/set_host RPC")
	}

	// 消费 gateway 事件
	go d.eventLoop(ctx)

	<-ctx.Done()
	d.shuttingDown = true
	if d.gw != nil {
		d.gw.Stop()
	}
	return nil
}

// connectGateway 启动 daemon.mjs 子进程
func (d *Daemon) connectGateway(ctx context.Context) {
	if d.passcode == "" || d.host == "" {
		d.broadcast(map[string]any{"method": "status", "message": "host/passcode not configured"})
		return
	}

	d.mu.Lock()
	d.connected = false
	d.reconnectN++
	n := d.reconnectN
	d.mu.Unlock()

	d.broadcast(map[string]any{"method": "connecting"})

	if d.gw != nil {
		d.gw.Stop()
	}

	gw := gateway.New(gateway.Config{
		Host:     d.host,
		Passcode: d.passcode,
		JsDir:    d.cfg.JsDir,
		NodeBin:  d.cfg.NodeBin,
	}, d.logger)

	if err := gw.Start(ctx); err != nil {
		d.logger.Error("gateway start failed", "error", err, "attempt", n)
		d.broadcast(map[string]any{"method": "disconnected", "error": err.Error()})
		d.scheduleReconnect(ctx)
		return
	}

	d.gw = gw
	d.gwEvents = gw.Events()

	// daemon.mjs stdin 模式启动后会自动连接，等 connected 事件
	d.logger.Info("gateway subprocess started, waiting for connection")
}

// eventLoop 消费 gateway 事件
func (d *Daemon) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-d.gwEvents:
			if !ok {
				return
			}
			d.handleGatewayEvent(ctx, evt)
		}
	}
}

func (d *Daemon) handleGatewayEvent(ctx context.Context, evt gateway.Event) {
	switch evt.Method {
	case "connected":
		d.mu.Lock()
		d.connected = true
		d.reconnectN = 0
		d.mu.Unlock()
		d.broadcast(map[string]any{"method": "connected"})
		d.logger.Info("gateway connected")

	case "disconnected":
		d.mu.Lock()
		d.connected = false
		authFailed := evt.Error != "" && (contains(evt.Error, "expired") || contains(evt.Error, "Authentication"))
		d.mu.Unlock()
		d.broadcast(map[string]any{"method": "disconnected", "error": evt.Error})
		if authFailed {
			d.logger.Warn("auth failed, waiting for new passcode", "error", evt.Error)
		} else {
			d.scheduleReconnect(ctx)
		}

	case "passcode_updated", "passcode_saved", "status", "connecting":
		d.broadcast(evt.Raw) // 原样转发

	default:
		d.broadcast(evt.Raw)
	}
}

// scheduleReconnect 指数退避
func (d *Daemon) scheduleReconnect(ctx context.Context) {
	d.mu.RLock()
	n := d.reconnectN
	d.mu.RUnlock()
	if n > maxReconnect {
		d.logger.Warn("max reconnect attempts reached, waiting for new passcode")
		d.broadcast(map[string]any{
			"method":  "disconnected",
			"error":   fmt.Sprintf("reconnect failed after %d attempts — passcode may have expired", maxReconnect),
		})
		return
	}
	delay := reconnectMin * time.Duration(1<<(n-1))
	if delay > reconnectMax {
		delay = reconnectMax
	}
	d.logger.Info("reconnect scheduled", "delay", delay, "attempt", n)
	go func() {
		select {
		case <-time.After(delay):
			d.connectGateway(ctx)
		case <-ctx.Done():
		}
	}()
}

// passcodePoller 轮询 passcode 文件
func (d *Daemon) passcodePoller(ctx context.Context) {
	t := time.NewTicker(passcodePoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c := readFile(d.passcodeFile)
			if c == "" || c == d.passcode {
				continue
			}
			d.mu.Lock()
			d.passcode = c
			d.mu.Unlock()
			d.logger.Info("passcode updated from file")
			d.broadcast(map[string]any{"method": "passcode_updated"})
			if d.gw != nil {
				d.gw.Stop()
			}
			d.reconnectN = 0
			d.connectGateway(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// keepaliveLoop 定期心跳（已由 daemon.mjs 内部 keepalive 处理，Go 侧仅做监控）
func (d *Daemon) keepaliveLoop(ctx context.Context) {
	t := time.NewTicker(keepaliveInt)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !d.isConnected() || d.gw == nil {
				continue
			}
			_, err := d.gw.Call("ping", nil, 5*time.Second)
			if err != nil {
				d.logger.Warn("keepalive failed", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// === TCP 服务器 ===

func (d *Daemon) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				d.logger.Error("accept", "error", err)
				continue
			}
		}
		go d.handleConn(ctx, conn)
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	id := fmt.Sprintf("%d_%s", time.Now().UnixNano(), conn.RemoteAddr())
	d.mu.Lock()
	d.tcpClients[id] = conn
	d.mu.Unlock()
	defer func() {
		conn.Close()
		d.mu.Lock()
		delete(d.tcpClients, id)
		remaining := len(d.tcpClients)
		d.mu.Unlock()
		d.logger.Info("client disconnected", "id", id, "remaining", remaining)
	}()

	d.logger.Info("client connected", "id", id)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req RPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			d.sendConn(conn, RPCResponse{Error: "invalid JSON"})
			continue
		}
		resp := d.handleRequest(ctx, req)
		if req.ID != "" {
			d.sendConn(conn, resp)
		}
	}
}

// handleRequest 路由请求
func (d *Daemon) handleRequest(ctx context.Context, req RPCRequest) RPCResponse {
	switch req.Method {
	case "set_passcode":
		return d.rpcSetPasscode(ctx, req)
	case "set_host":
		return d.rpcSetHost(ctx, req)
	case "ping":
		return RPCResponse{ID: req.ID, Result: map[string]any{
			"pong": true, "connected": d.isConnected(),
			"passcode_set": d.passcode != "", "host": d.host,
		}}
	case "get_config":
		return RPCResponse{ID: req.ID, Result: map[string]any{
			"host": d.host, "passcode_set": d.passcode != "",
			"connected": d.isConnected(), "tcp_addr": d.cfg.TCPAddr,
		}}
	default:
		return d.forwardToGateway(req)
	}
}

func (d *Daemon) rpcSetPasscode(ctx context.Context, req RPCRequest) RPCResponse {
	var p struct{ Passcode string `json:"passcode"` }
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Passcode == "" {
		return RPCResponse{ID: req.ID, Error: "passcode required"}
	}
	d.mu.Lock()
	d.passcode = p.Passcode
	d.reconnectN = 0
	d.mu.Unlock()
	os.WriteFile(d.passcodeFile, []byte(p.Passcode), 0600)
	d.logger.Info("passcode set")

	if d.gw != nil {
		d.gw.Stop()
	}
	d.connectGateway(ctx)
	return RPCResponse{ID: req.ID, Result: map[string]string{"status": "passcode_set"}}
}

func (d *Daemon) rpcSetHost(ctx context.Context, req RPCRequest) RPCResponse {
	var p struct{ Host string `json:"host"` }
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Host == "" {
		return RPCResponse{ID: req.ID, Error: "host required"}
	}
	d.mu.Lock()
	d.host = p.Host
	d.reconnectN = 0
	d.mu.Unlock()
	os.WriteFile(d.hostFile, []byte(p.Host), 0644)
	d.logger.Info("host set", "host", p.Host)

	if d.gw != nil {
		d.gw.Stop()
	}
	d.connectGateway(ctx)
	return RPCResponse{ID: req.ID, Result: map[string]string{"status": "host_set", "host": p.Host}}
}

// forwardToGateway 转发请求到 daemon.mjs
func (d *Daemon) forwardToGateway(req RPCRequest) RPCResponse {
	if !d.isConnected() || d.gw == nil {
		return RPCResponse{ID: req.ID, Error: "Not connected. Use set_passcode first."}
	}
	// 直接透传方法名和参数 — daemon.mjs stdin 模式已接受这些方法名
	result, err := d.gw.Call(req.Method, req.Params, rpcTimeout)
	if err != nil {
		return RPCResponse{ID: req.ID, Error: err.Error()}
	}
	return RPCResponse{ID: req.ID, Result: json.RawMessage(result)}
}

// === 工具方法 ===

func (d *Daemon) isConnected() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.connected
}

func (d *Daemon) broadcast(obj any) {
	data, err := json.Marshal(obj)
	if err != nil {
		return
	}
	data = append(data, '\n')
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, c := range d.tcpClients {
		c.Write(data)
	}
}

func (d *Daemon) sendConn(conn net.Conn, resp RPCResponse) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Write(data)
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
