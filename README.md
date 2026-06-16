# mihome-gateway-mcp

Go 重写的米家中枢网关极客版 MCP Server + Daemon。

## 架构

```
任意 AI Agent → MCP stdio → mgw-mcp (Go)
    → TCP :19345 → mgwd (Go)
    → stdin/stdout → daemon.mjs → gateway.js
    → WebSocket ECJPAKE+AES-GCM → 网关
```

**Phase 1（当前）**：Go daemon (`mgwd`) + Go MCP server (`mgw-mcp`)，保留 gateway.js + Node.js
**Phase 2（计划）**：Go 原生重写 ECJPAKE + AES-GCM 协议层，消除 Node.js 依赖

## 快速开始

### 构建

```bash
# 需要 Go 1.25+ 和 Node.js 18+
make build
```

### 运行

```bash
# 启动 daemon
./bin/mgwd --host 192.168.1.x --jsdir ./mi_gateway_js

# 设置密码
echo '{"id":"1","method":"set_passcode","params":{"passcode":"123456"}}' | nc 127.0.0.1 19345
```

### Hermes 配置

```yaml
# ~/.hermes/config.yaml
mcp:
  servers:
    mihome:
      command: /path/to/bin/mgw-mcp
      args: ["--daemon-addr", "127.0.0.1:19345"]
```

### systemd 安装

```bash
make install
systemctl --user enable --now mgwd
```

## MCP 工具

| 工具 | 说明 |
|------|------|
| `list_devices` | 列出所有设备 |
| `get_device_state` | 获取设备状态 |
| `list_scenes` | 列出所有场景 |
| `get_scene_graph` | 获取场景图数据 |
| `execute_scene` | 执行场景 |
| `gateway_status` | 网关连接状态 |
| `set_passcode` | 设置密码 |
| `set_host` | 设置网关 IP |
| `get_vars` / `set_var` | 自动化变量 |
| `call_api` | 透传任意 RPC |

## 项目结构

```
mihome-gateway-mcp/
├── cmd/mgwd/           # daemon 入口
├── cmd/mgw-mcp/        # MCP server 入口
├── internal/
│   ├── daemon/         # TCP JSON-RPC 服务器
│   └── gateway/        # gateway.js 子进程管理
├── mi_gateway_js/      # gateway.js + daemon.mjs
├── deploy/             # systemd service
└── Makefile
```

## License

MIT
