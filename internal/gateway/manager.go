// Package gateway 管理 daemon.mjs Node.js 子进程的生命周期
//
// 架构（Phase 1）：
//
//	TCP 客户端 → Go daemon (:19345) → stdin/stdout JSON-RPC → daemon.mjs → gateway.js → 网关
//
// Go daemon 启动 daemon.mjs（不带 --tcp 标志，使用 stdin/stdout 模式），
// 通过 stdin 发送请求，从 stdout 读取响应和事件。
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Event 代表 daemon.mjs 子进程发回的事件/响应
type Event struct {
	Method  string          `json:"method,omitempty"` // 事件类型
	ID      string          `json:"id,omitempty"`     // RPC 响应 ID
	Message string          `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Host    string          `json:"host,omitempty"`
	Port    int             `json:"port,omitempty"`
	Raw     json.RawMessage `json:"-"` // 完整原始 JSON
}

// Config 配置子进程
type Config struct {
	Host     string // 网关 IP
	Passcode string // 6 位动态密码
	JsDir    string // daemon.mjs 所在目录（含 node_modules）
	NodeBin  string // node 可执行文件路径（空则自动查找）
}

// Manager 管理 daemon.mjs 子进程
type Manager struct {
	cfg    Config
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu      sync.RWMutex
	running bool
	stdin   io.WriteCloser

	// 事件/响应分发
	eventCh    chan Event       // 非 RPC 响应的事件
	pendingMu  sync.Mutex
	pendingMap map[string]chan Event // id → 响应 channel

	logger *slog.Logger
}

// New 创建 Manager 实例
func New(cfg Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:        cfg,
		eventCh:    make(chan Event, 64),
		pendingMap: make(map[string]chan Event),
		logger:     logger,
	}
}

// Events 返回事件 channel（连接状态变化等非 RPC 响应事件）
func (m *Manager) Events() <-chan Event {
	return m.eventCh
}

// Start 启动 daemon.mjs 子进程（stdin/stdout 模式）
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return fmt.Errorf("gateway already running")
	}

	jsDir := m.cfg.JsDir
	if jsDir == "" {
		return fmt.Errorf("JsDir not configured — daemon.mjs directory required")
	}

	nodeBin := m.cfg.NodeBin
	if nodeBin == "" {
		var err error
		nodeBin, err = exec.LookPath("node")
		if err != nil {
			return fmt.Errorf("node not found in PATH: %w", err)
		}
	}

	daemonPath := filepath.Join(jsDir, "daemon.mjs")
	if _, err := os.Stat(daemonPath); os.IsNotExist(err) {
		return fmt.Errorf("daemon.mjs not found at %s", daemonPath)
	}

	ctx, m.cancel = context.WithCancel(ctx)
	// 不带 --tcp 标志，使用 stdin/stdout 模式
	cmd := exec.CommandContext(ctx, nodeBin, daemonPath)
	cmd.Dir = jsDir
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if m.cfg.Host != "" {
		cmd.Env = append(cmd.Env, "MGW_HOST="+m.cfg.Host)
	}
	if m.cfg.Passcode != "" {
		cmd.Env = append(cmd.Env, "MGW_PASSCODE="+m.cfg.Passcode)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	m.cmd = cmd
	m.running = true
	m.stdin = stdin

	// 读取 stdout → 分发事件/响应
	go m.readLoop(stdout)
	// stderr → 日志
	go m.pipeLog(stderr, "daemon.mjs")

	m.logger.Info("daemon.mjs started", "pid", cmd.Process.Pid, "dir", jsDir)
	return nil
}

// Call 发送 JSON-RPC 请求并等待响应
func (m *Manager) Call(method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	m.mu.RLock()
	running := m.running
	m.mu.RUnlock()
	if !running {
		return nil, fmt.Errorf("gateway not running")
	}

	// 构造请求
	id := fmt.Sprintf("go_%d", time.Now().UnixNano())
	req := map[string]interface{}{
		"id":     id,
		"method": method,
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// 注册 pending 响应
	ch := make(chan Event, 1)
	m.pendingMu.Lock()
	m.pendingMap[id] = ch
	m.pendingMu.Unlock()
	defer func() {
		m.pendingMu.Lock()
		delete(m.pendingMap, id)
		m.pendingMu.Unlock()
	}()

	// 发送
	data = append(data, '\n')
	m.mu.RLock()
	_, err = m.stdin.Write(data)
	m.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	// 等待响应
	select {
	case evt := <-ch:
		if evt.Error != "" {
			return nil, fmt.Errorf("rpc error: %s", evt.Error)
		}
		return evt.Result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("rpc timeout (%v)", timeout)
	}
}

// SendJSON 发送原始 JSON（用于广播等不需要响应的场景）
func (m *Manager) SendJSON(data []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stdin == nil {
		return fmt.Errorf("gateway not running")
	}
	data = append(data, '\n')
	_, err := m.stdin.Write(data)
	return err
}

// Stop 停止子进程
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- m.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			m.cmd.Process.Kill()
		}
	}
	m.running = false
	m.stdin = nil

	// 清理 pending — 发送错误而非 close，避免接收方拿到零值
	m.pendingMu.Lock()
	for id, ch := range m.pendingMap {
		ch <- Event{Error: "gateway stopped"}
		delete(m.pendingMap, id)
	}
	m.pendingMu.Unlock()

	m.logger.Info("daemon.mjs stopped")
}

// IsRunning 返回子进程是否在运行
func (m *Manager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// readLoop 从 stdout 逐行读取 JSON，分发到 pending 或 eventCh
func (m *Manager) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var evt Event
		if err := json.Unmarshal(line, &evt); err != nil {
			m.logger.Debug("stdout non-JSON", "line", string(line))
			continue
		}
		evt.Raw = json.RawMessage(append([]byte(nil), line...))

		// 有 ID → RPC 响应，分发到 pending
		if evt.ID != "" {
			m.pendingMu.Lock()
			ch, ok := m.pendingMap[evt.ID]
			if ok {
				delete(m.pendingMap, evt.ID)
			}
			m.pendingMu.Unlock()
			if ok {
				select {
				case ch <- evt:
				default:
				}
				continue
			}
		}

		// 无 ID → 事件（connected, disconnected, status 等）
		select {
		case m.eventCh <- evt:
		default:
			m.logger.Warn("event channel full, dropping", "method", evt.Method)
		}
	}

	// stdout 关闭 = 进程退出
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
	if err := scanner.Err(); err != nil {
		m.logger.Error("stdout read error", "error", err)
	}
	m.logger.Info("daemon.mjs stdout closed")
}

// pipeLog 将 stderr 作为日志输出
func (m *Manager) pipeLog(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		m.logger.Info(prefix, "msg", scanner.Text())
	}
}
