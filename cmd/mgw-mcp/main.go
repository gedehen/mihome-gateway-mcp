// mgw-mcp — Go MCP Server for Mi Home Gateway
//
// 通过 TCP JSON-RPC 连接到 mgwd，暴露 MCP 工具给 AI agent。
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
	// === 连接管理 ===
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
		mcp.NewTool("gateway_status",
			mcp.WithDescription("查询网关连接状态"),
		),
		handleGatewayStatus,
	)

	// === 设备操作 ===
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
		mcp.NewTool("device_specs",
			mcp.WithDescription("获取设备详细规格和能力"),
			mcp.WithString("did", mcp.Description("设备 DID（可选，不填返回所有设备）")),
		),
		handleDeviceSpecs,
	)

	// === 场景操作 ===
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
		mcp.NewTool("create_scene",
			mcp.WithDescription("创建新场景"),
			mcp.WithString("name", mcp.Required(), mcp.Description("场景名称")),
			mcp.WithString("nodes_json", mcp.Required(), mcp.Description("节点 JSON 数组")),
		),
		handleCreateScene,
	)

	s.AddTool(
		mcp.NewTool("delete_scene",
			mcp.WithDescription("删除场景"),
			mcp.WithString("scene_id", mcp.Required(), mcp.Description("场景 ID")),
		),
		handleDeleteScene,
	)

	s.AddTool(
		mcp.NewTool("toggle_scene",
			mcp.WithDescription("启用/禁用场景"),
			mcp.WithString("scene_id", mcp.Required(), mcp.Description("场景 ID")),
			mcp.WithString("enable", mcp.Required(), mcp.Description("true=启用, false=禁用")),
		),
		handleToggleScene,
	)

	s.AddTool(
		mcp.NewTool("execute_scene",
			mcp.WithDescription("手动执行一个场景"),
			mcp.WithString("scene_id", mcp.Required(), mcp.Description("场景 ID")),
		),
		handleExecuteScene,
	)

	// === 变量操作 ===
	s.AddTool(
		mcp.NewTool("get_variables",
			mcp.WithDescription("获取自动化变量列表"),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleGetVariables,
	)

	s.AddTool(
		mcp.NewTool("set_variable",
			mcp.WithDescription("设置自动化变量值"),
			mcp.WithString("name", mcp.Required(), mcp.Description("变量名")),
			mcp.WithString("value", mcp.Required(), mcp.Description("变量值")),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleSetVariable,
	)

	s.AddTool(
		mcp.NewTool("create_variable",
			mcp.WithDescription("创建自动化变量"),
			mcp.WithString("name", mcp.Required(), mcp.Description("变量名")),
			mcp.WithString("value", mcp.Required(), mcp.Description("初始值")),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleCreateVariable,
	)

	s.AddTool(
		mcp.NewTool("delete_variable",
			mcp.WithDescription("删除自动化变量"),
			mcp.WithString("name", mcp.Required(), mcp.Description("变量名")),
			mcp.WithString("scope", mcp.Description("变量作用域，默认 global")),
		),
		handleDeleteVariable,
	)

	// === 备份操作 ===
	s.AddTool(
		mcp.NewTool("list_backups",
			mcp.WithDescription("列出所有备份"),
		),
		handleListBackups,
	)

	s.AddTool(
		mcp.NewTool("create_backup",
			mcp.WithDescription("创建备份"),
		),
		handleCreateBackup,
	)

	s.AddTool(
		mcp.NewTool("restore_backup",
			mcp.WithDescription("恢复备份"),
			mcp.WithString("backup_id", mcp.Required(), mcp.Description("备份 ID")),
		),
		handleRestoreBackup,
	)

	// === 透传 ===
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

// --- 连接管理 ---

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

func handleGatewayStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("ping", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

// --- 设备操作 ---

func handleListDevices(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("devices", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// 格式化输出：✓ 设备名 | Room | 型号
	var data struct {
		DevList map[string]struct {
			Name      string `json:"name"`
			Model     string `json:"model"`
			ModelName string `json:"modelName"`
			RoomName  string `json:"roomName"`
			Online    bool   `json:"online"`
			URN       string `json:"urn"`
		} `json:"devList"`
	}
	if err := json.Unmarshal(result, &data); err != nil {
		return formatResult(result), nil
	}

	var lines []string
	for did, d := range data.DevList {
		status := "✓"
		if !d.Online {
			status = "✗"
		}
		name := d.Name
		if name == "" {
			name = d.ModelName
		}
		line := fmt.Sprintf("%s %s | Room: %s | %s", status, name, d.RoomName, d.Model)
		lines = append(lines, line)
		_ = did
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("No devices."), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func handleGetDeviceState(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	did, _ := req.RequireString("did")
	result, err := daemonCall("get_device_state", map[string]any{"did": did})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleDeviceSpecs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// 获取设备列表
	result, err := daemonCall("devices", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var data struct {
		DevList map[string]struct {
			Name      string `json:"name"`
			Model     string `json:"model"`
			ModelName string `json:"modelName"`
			RoomName  string `json:"roomName"`
			URN       string `json:"urn"`
		} `json:"devList"`
	}
	if err := json.Unmarshal(result, &data); err != nil {
		return mcp.NewToolResultError("parse error: " + err.Error()), nil
	}

	didFilter := req.GetString("did", "")
	var lines []string
	for did, d := range data.DevList {
		if didFilter != "" && did != didFilter {
			continue
		}
		name := d.Name
		if name == "" {
			name = d.ModelName
		}
		caps := getDeviceCapabilities(d.URN)
		line := fmt.Sprintf("[%s] %s (%s)", did, name, caps["name"])
		if d.RoomName != "" {
			line += fmt.Sprintf(" @ %s", d.RoomName)
		}
		if props, ok := caps["props"]; ok && props != "" {
			line += fmt.Sprintf("\n    Props: %s", props)
		}
		if events, ok := caps["events"]; ok && events != "" {
			line += fmt.Sprintf("\n    Events: %s", events)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("No devices found."), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

// --- 场景操作 ---

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

func handleCreateScene(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	nodesJSON, _ := req.RequireString("nodes_json")

	var nodes []json.RawMessage
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		return mcp.NewToolResultError("invalid nodes_json: " + err.Error()), nil
	}

	sceneID := fmt.Sprintf("%d", time.Now().UnixMilli())
	graph := map[string]any{
		"id":    sceneID,
		"nodes": nodes,
		"cfg": map[string]any{
			"id": sceneID,
			"userData": map[string]any{
				"name":           name,
				"transform":      map[string]any{"x": 0, "y": 0, "scale": 0.5, "rotate": 0},
				"lastUpdateTime": time.Now().UnixMilli(),
				"version":        0,
			},
			"uiType": "normal",
			"enable": true,
		},
	}

	_, err := daemonCall("set_graph", graph)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 场景创建成功: %s (id=%s)", name, sceneID)), nil
}

func handleDeleteScene(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sceneID, _ := req.RequireString("scene_id")
	_, err := daemonCall("delete_graph", map[string]any{"id": sceneID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 场景已删除: %s", sceneID)), nil
}

func handleToggleScene(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sceneID, _ := req.RequireString("scene_id")
	enableStr, _ := req.RequireString("enable")
	enable := enableStr == "true"

	_, err := daemonCall("change_graph_config", map[string]any{
		"graphId": sceneID,
		"config":  map[string]any{"enable": enable},
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	status := "启用"
	if !enable {
		status = "禁用"
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 场景已%s: %s", status, sceneID)), nil
}

func handleExecuteScene(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sceneID, _ := req.RequireString("scene_id")
	_, err := daemonCall("execute_scene", map[string]any{"scene_id": sceneID, "start": true})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("Scene executed."), nil
}

// --- 变量操作 ---

func handleGetVariables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "global")
	result, err := daemonCall("get_vars", map[string]any{"scope": scope})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleSetVariable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	value, _ := req.RequireString("value")
	scope := req.GetString("scope", "global")
	_, err := daemonCall("set_var", map[string]any{
		"name": name, "value": value, "scope": scope,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 变量已设置: %s = %s", name, value)), nil
}

func handleCreateVariable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	value, _ := req.RequireString("value")
	scope := req.GetString("scope", "global")
	_, err := daemonCall("create_var", map[string]any{
		"id": name, "value": value, "scope": scope,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 变量已创建: %s = %s", name, value)), nil
}

func handleDeleteVariable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	scope := req.GetString("scope", "global")
	_, err := daemonCall("delete_var", map[string]any{
		"id": name, "scope": scope,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 变量已删除: %s", name)), nil
}

// --- 备份操作 ---

func handleListBackups(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := daemonCall("get_backup_list", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return formatResult(result), nil
}

func handleCreateBackup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	_, err := daemonCall("create_backup", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("✅ 备份创建成功"), nil
}

func handleRestoreBackup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	backupID, _ := req.RequireString("backup_id")
	_, err := daemonCall("load_backup", map[string]any{"id": backupID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ 备份已恢复: %s", backupID)), nil
}

// --- 透传 ---

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

// === MiOT 设备规格数据库 ===

// getDeviceCapabilities 从 URN 获取设备能力
func getDeviceCapabilities(urn string) map[string]string {
	// URN 格式: urn:miot-spec-v2:device:lock:0000A038:xiaomi-s20pro:2
	parts := strings.Split(urn, ":")
	if len(parts) < 5 {
		return map[string]string{"name": "unknown"}
	}
	typeCode := parts[4]
	if caps, ok := deviceTypes[typeCode]; ok {
		return caps
	}
	return map[string]string{"name": parts[3]}
}

// deviceTypes 设备类型数据库（对标 Python device_specs.py）
var deviceTypes = map[string]map[string]string{
	"0000A001": {"name": "灯", "props": "on, brightness, color-temperature, mode"},
	"0000A002": {"name": "插座", "props": "on, power, voltage, current"},
	"0000A003": {"name": "开关", "props": "on", "events": "click, double_click, long_press"},
	"0000A007": {"name": "空气净化器", "props": "on, fan-speed, pm2.5, mode, filter-life"},
	"0000A009": {"name": "电水壶", "props": "on, temperature, keep-warm"},
	"0000A00A": {"name": "温湿度计", "props": "temperature, humidity"},
	"0000A00B": {"name": "电饭煲", "props": "on, mode, keep-warm"},
	"0000A00C": {"name": "窗帘", "props": "on, position"},
	"0000A00D": {"name": "晾衣架", "props": "on, lift-position, light"},
	"0000A015": {"name": "智能音箱", "props": "on, volume, play-state", "actions": "play-tts, play-music"},
	"0000A016": {"name": "门窗传感器", "props": "contact", "events": "open, close"},
	"0000A019": {"name": "网关"},
	"0000A01B": {"name": "净烟机", "props": "on, fan-speed, light"},
	"0000A01C": {"name": "摄像机", "props": "on, recording"},
	"0000A01F": {"name": "洗衣机", "props": "on, program, status"},
	"0000A021": {"name": "无线开关", "events": "click, double_click, long_press"},
	"0000A024": {"name": "水浸传感器", "props": "leak", "events": "leak"},
	"0000A028": {"name": "浴霸", "props": "on, fan-speed, light, ventilation"},
	"0000A02A": {"name": "燃气热水器", "props": "on, temperature"},
	"0000A02E": {"name": "智能马桶", "props": "on, seat-temperature"},
	"0000A034": {"name": "洗碗机", "props": "on, program, status"},
	"0000A038": {"name": "智能门锁", "events": "unlock, lock, doorbell"},
	"0000A067": {"name": "宠物饮水机", "props": "on, water-level"},
	"0000A069": {"name": "水暖毯", "props": "on, temperature"},
	"0000A07D": {"name": "体脂秤", "props": "weight, body-fat"},
	"0000A083": {"name": "按摩仪", "props": "on, mode"},
	"0000A0A4": {"name": "空气炸锅", "props": "on, temperature, time, mode"},
	"0000A0A8": {"name": "智能沙发", "props": "on, position"},
	"0000A099": {"name": "中控屏"},
	"0000A0BF": {"name": "人体存在传感器", "props": "occupancy, illumination"},
	"0000A0EF": {"name": "笔记本"},
}
