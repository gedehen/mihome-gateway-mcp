// mgw-mcp — Go MCP Server for Mi Home Gateway
//
// 通过 TCP JSON-RPC 连接到 mgwd，暴露 MCP 工具给 AI agent。
//
// 用法:
//
//	mgw-mcp                              # stdio 模式（Hermes/agent 直接调用）
//	mgw-mcp --daemon-addr 127.0.0.1:19345
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var (
	flagDaemonAddr = flag.String("daemon-addr", "127.0.0.1:19345", "daemon TCP address")
	flagVerbose    = flag.Bool("v", false, "verbose logging")
)

var logger *slog.Logger

func main() {
	flag.Parse()

	level := slog.LevelInfo
	if *flagVerbose {
		level = slog.LevelDebug
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	s := server.NewMCPServer(
		"mihome-gateway",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	registerTools(s)

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	if err := server.ServeStdio(s); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// registerTools 注册所有 MCP 工具
func registerTools(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("list_devices",
			mcp.WithDescription("列出网关上所有设备"),
		),
		handleListDevices,
	)

	s.AddTool(
		mcp.NewTool("get_device_state",
			mcp.WithDescription("获取设备当前状态"),
			mcp.WithString("did", mcp.Required(), mcp.Description("设备 DID")),
		),
		handleGetDeviceState,
	)

	s.AddTool(
		mcp.NewTool("list_scenes",
			mcp.WithDescription("列出所有自动化场景"),
		),
		handleListScenes,
	)

	s.AddTool(
		mcp.NewTool("get_scene_graph",
			mcp.WithDescription("获取场景图数据（节点和连接）"),
			mcp.WithString("scene_id", mcp.Required(), mcp.Description("场景 ID")),
		),
		handleGetSceneGraph,
	)

	s.AddTool(
		mcp.NewTool("execute_scene",
			mcp.WithDescription("手动执行一个场景"),
			mcp.WithString("scene_id", mcp.Required(), mcp.Description("场景 ID")),
		),
		handleExecuteScene,
	)

	s.AddTool(
		mcp.NewTool("gateway_status",
			mcp.WithDescription("查询网关连接状态"),
		),
		handleGatewayStatus,
	)

	s.AddTool(
		mcp.NewTool("set_passcode",
			mcp.WithDescription("设置网关动态密码并重连"),
			mcp.WithString("passcode", mcp.Required(), mcp.Description("6 位动态密码")),
		),
		handleSetPasscode,
	)

	s.AddTool(
		mcp.NewTool("set_host",
			mcp.WithDescription("设置网关 IP 地址并重连"),
			mcp.WithString("host", mcp.Required(), mcp.Description("网关 IP 地址")),
		),
		handleSetHost,
	)

	s.AddTool(
		mcp.NewTool("get_vars",
			mcp.WithDescription("获取自动化变量列表"),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleGetVars,
	)

	s.AddTool(
		mcp.NewTool("set_var",
			mcp.WithDescription("设置自动化变量值"),
			mcp.WithString("name", mcp.Required(), mcp.Description("变量名")),
			mcp.WithString("value", mcp.Required(), mcp.Description("变量值")),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleSetVar,
	)

	s.AddTool(
		mcp.NewTool("call_api",
			mcp.WithDescription("透传任意 JSON-RPC 方法到网关（高级用法）"),
			mcp.WithString("method", mcp.Required(), mcp.Description("RPC 方法名")),
			mcp.WithString("params", mcp.Description("JSON 参数")),
		),
		handleCallAPI,
	)
}

// === MCP 工具处理函数 ===

func handleListDevices(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("devices", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleGetDeviceState(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	did, _ := req.RequireString("did")
	result, err := daemonCall("get_device_state", map[string]any{"did": did})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleListScenes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("scenes", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleGetSceneGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sceneID, _ := req.RequireString("scene_id")
	result, err := daemonCall("get_graph", map[string]any{"graphId": sceneID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleExecuteScene(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sceneID, _ := req.RequireString("scene_id")
	result, err := daemonCall("execute_scene", map[string]any{"scene_id": sceneID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleGatewayStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("ping", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleSetPasscode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	passcode, _ := req.RequireString("passcode")
	result, err := daemonCall("set_passcode", map[string]any{"passcode": passcode})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleSetHost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	host, _ := req.RequireString("host")
	result, err := daemonCall("set_host", map[string]any{"host": host})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleGetVars(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "global")
	result, err := daemonCall("get_vars", map[string]any{"scope": scope})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleSetVar(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	value, _ := req.RequireString("value")
	scope := req.GetString("scope", "global")
	result, err := daemonCall("set_var", map[string]any{
		"name": name, "value": value, "scope": scope,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleCallAPI(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method, _ := req.RequireString("method")
	paramsStr := req.GetString("params", "{}")

	var params json.RawMessage
	if err := json.Unmarshal([]byte(paramsStr), &params); err != nil {
		return mcp.NewToolResultError("invalid params JSON: " + err.Error()), nil
	}

	result, err := daemonCall(method, params)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

// === daemon TCP 通信 ===

func daemonCall(method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("tcp", *flagDaemonAddr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable at %s — is mgwd running? %w", *flagDaemonAddr, err)
	}
	defer conn.Close()

	req := map[string]any{
		"id":     fmt.Sprintf("mcp_%d", time.Now().UnixNano()),
		"method": method,
	}
	if params != nil {
		req["params"] = params
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write to daemon: %w", err)
	}

	// 用 scanner 逐行读取，取最后一个有 id 的响应
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	var lastResp *struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var resp struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err == nil && resp.ID != "" {
			lastResp = &resp
		}
	}

	if lastResp == nil {
		return nil, fmt.Errorf("no valid response from daemon")
	}
	if lastResp.Error != "" {
		return nil, fmt.Errorf("daemon error: %s", lastResp.Error)
	}
	return lastResp.Result, nil
}

func formatResult(data json.RawMessage) *mcp.CallToolResult {
	if data == nil {
		return mcp.NewToolResultText("null")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return mcp.NewToolResultText(string(data))
	}
	return mcp.NewToolResultText(pretty.String())
}
