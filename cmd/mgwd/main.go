// mgwd — Go 重写的 Mi Home Gateway Daemon
//
// --native 模式：Go 原生协议，零 Node.js 依赖
// 默认模式：通过 stdin/stdout 管理 daemon.mjs 子进程
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
	flagJsDir    = flag.String("jsdir", "", "daemon.mjs directory (for legacy mode)")
	flagNative   = flag.Bool("native", false, "use Go native protocol (no Node.js)")
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
		host = envOrFile("MGW_HOST", hostFilePath())
	}
	passcode := *flagPasscode
	if passcode == "" {
		passcode = envOrFile("MGW_PASSCODE", passcodeFilePath())
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sigCh; cancel() }()

	if *flagNative {
		runNative(ctx, host, passcode)
	} else {
		runLegacy(ctx, host, passcode)
	}
}

// === 方法名映射（对标 daemon.mjs handleRequest）===

type apiCall struct {
	Method  string
	Params  interface{}
	Timeout time.Duration
}

// dtype 规范化（对标 scene_builder.py _normalize_dtype）
var dtypeMap = map[string]string{
	"bool":    "boolean",
	"boolean": "boolean",
	"int":     "int",
	"uint8":   "uint8",
	"uint16":  "uint16",
	"float":   "float",
	"string":  "string",
}

func normalizeDtype(dt string) string {
	if v, ok := dtypeMap[dt]; ok {
		return v
	}
	return dt
}

// normalizeGraphParams 规范化 setGraph 参数中的 dtype 和 operator
func normalizeGraphParams(params json.RawMessage) interface{} {
	var graph map[string]interface{}
	if err := json.Unmarshal(params, &graph); err != nil {
		return rawParams(params)
	}

	nodes, ok := graph["nodes"].([]interface{})
	if !ok {
		return graph
	}

	for _, n := range nodes {
		node, ok := n.(map[string]interface{})
		if !ok {
			continue
		}
		props, ok := node["props"].(map[string]interface{})
		if !ok {
			continue
		}

		// 规范化 dtype
		if dt, ok := props["dtype"].(string); ok {
			props["dtype"] = normalizeDtype(dt)
		}

		// 规范化 operator（== → =）
		if op, ok := props["operator"].(string); ok {
			if op == "==" {
				props["operator"] = "="
			}
		}
	}

	return graph
}

func mapMethod(method string, params json.RawMessage) apiCall {
	const (
		defTimeout  = 10 * time.Second
		shortTimeout = 5 * time.Second
		longTimeout  = 15 * time.Second
		xlongTimeout = 30 * time.Second
	)

	switch method {
	case "auth":
		return apiCall{"getVarList", map[string]any{"scope": "global"}, shortTimeout}

	case "devices", "list_devices":
		return apiCall{"getDevList", nil, longTimeout}

	case "scenes", "list_scenes":
		return apiCall{"getGraphList", nil, longTimeout}

	case "get_graph":
		var p struct {
			GraphID  string `json:"graphId"`
			ID       string `json:"id"`
			GraphID2 string `json:"graph_id"`
		}
		json.Unmarshal(params, &p)
		gid := p.GraphID
		if gid == "" {
			gid = p.ID
		}
		if gid == "" {
			gid = p.GraphID2
		}
		return apiCall{"getGraph", map[string]any{"id": gid}, longTimeout}

	case "get_graph_list":
		return apiCall{"getGraphList", nil, longTimeout}

	case "delete_graph":
		return apiCall{"deleteGraph", rawParams(params), defTimeout}

	case "change_graph_config":
		return apiCall{"changeGraphConfig", rawParams(params), defTimeout}

	case "execute_scene":
		var p struct {
			SceneID string `json:"scene_id"`
			Start   *bool  `json:"start"`
		}
		json.Unmarshal(params, &p)
		start := true
		if p.Start != nil {
			start = *p.Start
		}
		return apiCall{"changeGraphConfig", map[string]any{
			"graphId": p.SceneID,
			"config":  map[string]any{"start": start},
		}, defTimeout}

	case "get_vars":
		var p struct{ Scope string `json:"scope"` }
		json.Unmarshal(params, &p)
		if p.Scope == "" {
			p.Scope = "global"
		}
		return apiCall{"getVarList", map[string]any{"scope": p.Scope}, shortTimeout}

	case "set_var":
		var p struct {
			Scope string `json:"scope"`
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
		json.Unmarshal(params, &p)
		if p.Scope == "" {
			p.Scope = "global"
		}
		return apiCall{"setVarValue", map[string]any{
			"scope": p.Scope, "id": p.Name, "value": p.Value,
		}, shortTimeout}

	case "set_graph":
		// 规范化 dtype 和 operator
		return apiCall{"setGraph", normalizeGraphParams(params), defTimeout}

	case "device_specs_extra":
		return apiCall{"getDevList", nil, longTimeout}

	// === 备份管理 ===
	case "get_backup_list":
		return apiCall{"getBackupList", map[string]any{"from": "fds"}, longTimeout}
	case "create_backup":
		return apiCall{"createBackup", wrapFds(params), xlongTimeout}
	case "generate_backup":
		return apiCall{"generateBackup", wrapFds(params), xlongTimeout}
	case "download_backup":
		return apiCall{"downloadBackup", wrapFds(params), longTimeout}
	case "load_backup":
		return apiCall{"loadBackup", rawParams(params), xlongTimeout}
	case "delete_backup":
		return apiCall{"deleteBackup", wrapFds(params), defTimeout}
	case "get_backup_progress":
		return apiCall{"getBackupProgress", wrapFds(params), longTimeout}
	case "get_backup_config":
		return apiCall{"getBackupConfig", map[string]any{"from": "fds"}, longTimeout}
	case "set_backup_config":
		return apiCall{"setBackupConfig", wrapFds(params), defTimeout}

	// === 日志 ===
	case "get_log":
		return apiCall{"getLog", rawParams(params), longTimeout}

	// === 变量高级 CRUD ===
	case "create_var":
		return apiCall{"createVar", rawParams(params), shortTimeout}
	case "delete_var":
		return apiCall{"deleteVar", rawParams(params), shortTimeout}
	case "get_var_config":
		return apiCall{"getVarConfig", rawParams(params), shortTimeout}
	case "set_var_config":
		return apiCall{"setVarConfig", rawParams(params), shortTimeout}
	case "get_var_value":
		return apiCall{"getVarValue", rawParams(params), shortTimeout}
	case "get_var_scope_list":
		return apiCall{"getVarScopeList", nil, shortTimeout}

	default:
		return apiCall{method, rawParams(params), defTimeout}
	}
}

func wrapFds(params json.RawMessage) any {
	var p any
	json.Unmarshal(params, &p)
	return map[string]any{"from": "fds", "params": p}
}

func rawParams(params json.RawMessage) any {
	if len(params) == 0 {
		return nil
	}
	var p any
	json.Unmarshal(params, &p)
	return p
}

// === Native 模式 ===

func runNative(ctx context.Context, host, passcode string) {
	ln, err := net.Listen("tcp", *flagTCPAddr)
	if err != nil {
		logger.Error("listen failed", "error", err)
		os.Exit(1)
	}
	defer ln.Close()
	logger.Info("TCP listening (native mode)", "addr", *flagTCPAddr)

	var (
		conn     *native.Connection
		connMu   sync.RWMutex
		tcpConns = make(map[string]net.Conn)
		tcpMu    sync.RWMutex
	)

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
		broadcastTCP(&tcpMu, tcpConns, map[string]any{"method": "connected"})
	}

	if passcode != "" && host != "" {
		go connectGateway()
	}

	go pollPasscode(ctx, &passcode, func() { go connectGateway() })

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
			go handleNativeClient(ctx, c, &conn, &connMu, tcpConns, &tcpMu,
				&host, &passcode, connectGateway)
		}
	}()

	<-ctx.Done()
	connMu.Lock()
	if conn != nil {
		conn.Close()
	}
	connMu.Unlock()
}

func handleNativeClient(
	ctx context.Context, tc net.Conn,
	conn **native.Connection, connMu *sync.RWMutex,
	tcpConns map[string]net.Conn, tcpMu *sync.RWMutex,
	host, passcode *string,
	reconnect func(),
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
				writeFile(passcodeFilePath(), p.Passcode, 0600)
				resp = rpcResp(req.ID, map[string]string{"status": "passcode_set"})
				reconnect()
			}

		case "set_host":
			var p struct{ Host string `json:"host"` }
			json.Unmarshal(req.Params, &p)
			if p.Host != "" {
				*host = p.Host
				writeFile(hostFilePath(), p.Host, 0644)
				resp = rpcResp(req.ID, map[string]string{"status": "host_set", "host": p.Host})
				reconnect()
			}

		case "ping":
			connMu.RLock()
			connected := *conn != nil
			connMu.RUnlock()
			resp = rpcResp(req.ID, map[string]any{
				"pong": true, "connected": connected,
				"passcode_set": *passcode != "", "host": *host,
			})

		case "get_config":
			resp = rpcResp(req.ID, map[string]any{
				"host": *host, "passcode_set": *passcode != "",
				"connected": *conn != nil, "tcp_addr": *flagTCPAddr, "native": true,
			})

		case "dagre_layout":
			resp = rpcErr(req.ID, "dagre_layout not yet implemented in native mode")

		case "get_session_keys":
			resp = rpcErr(req.ID, "get_session_keys not yet implemented in native mode")

		default:
			connMu.RLock()
			c := *conn
			connMu.RUnlock()
			if c == nil {
				resp = rpcErr(req.ID, "Not connected. Use set_passcode first.")
			} else {
				call := mapMethod(req.Method, req.Params)
				result, err := c.Call(call.Method, call.Params, call.Timeout)
				if err != nil {
					resp = rpcErr(req.ID, err.Error())
				} else {
					resp = rpcResp(req.ID, json.RawMessage(result))
				}
			}
		}

		if req.ID != "" && resp != nil {
			sendJSON(tc, resp)
		}
	}
}

// === 共享工具函数 ===

func rpcResp(id string, result any) map[string]any {
	return map[string]any{"id": id, "result": result}
}

func rpcErr(id string, err string) map[string]any {
	return map[string]any{"id": id, "error": err}
}

func sendJSON(conn net.Conn, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Write(data)
}

func broadcastTCP(tcpMu *sync.RWMutex, tcpConns map[string]net.Conn, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	tcpMu.RLock()
	defer tcpMu.RUnlock()
	for _, c := range tcpConns {
		c.Write(data)
	}
}

func pollPasscode(ctx context.Context, passcode *string, onChange func()) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			newPC := readFileContent(passcodeFilePath())
			if newPC != "" && newPC != *passcode {
				*passcode = newPC
				logger.Info("passcode updated from file")
				onChange()
			}
		case <-ctx.Done():
			return
		}
	}
}

func envOrFile(envKey, filePath string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return readFileContent(filePath)
}

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

func writeFile(path string, content string, perm os.FileMode) {
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(content), perm)
}

func hermesHome() string {
	return filepath.Join(os.Getenv("HOME"), ".hermes")
}

func passcodeFilePath() string { return filepath.Join(hermesHome(), "mihome", "passcode") }
func hostFilePath() string     { return filepath.Join(hermesHome(), "mihome", "host") }

func runLegacy(ctx context.Context, host, passcode string) {
	logger.Error("legacy mode requires daemon package — use --native")
	os.Exit(1)
}
