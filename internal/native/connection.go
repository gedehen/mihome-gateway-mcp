// package native — 米家网关 Go 原生 WebSocket 连接
//
// 完整替代 daemon.mjs + gateway.js，直接与网关 WebSocket 通信。
package native

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// 数据类型常量
const (
	dataTypeProtocolList    = 1
	dataTypeSelectedProto   = 2
	dataTypeSessionKeyExch  = 3
	dataTypeError           = 4
	dataTypeData            = 5
	dataTypeECJPAKERoundOne = 32
	dataTypeECJPAKERoundTwo = 33
)

// Event 连接事件
type Event struct {
	Method string
	Error  string
	Raw    json.RawMessage
}

// Connection Go 原生 WebSocket 连接
type Connection struct {
	host     string
	passcode string
	logger   *slog.Logger

	ws     *websocket.Conn
	mu     sync.Mutex
	closed bool

	recvCipher *Cipher
	sendCipher *Cipher

	reqID     uint32
	pending   map[string]chan json.RawMessage
	pendingMu sync.Mutex

	eventCh chan Event

	// 原始二进制消息 channel（ECJPAKE/密钥交换用）
	rawCh chan rawMsg
}

type rawMsg struct {
	data []byte
	err  error
}

// NewConnection 创建连接
func NewConnection(host, passcode string, logger *slog.Logger) *Connection {
	if logger == nil {
		logger = slog.Default()
	}
	return &Connection{
		host:     host,
		passcode: passcode,
		logger:   logger,
		pending:  make(map[string]chan json.RawMessage),
		eventCh:  make(chan Event, 32),
		rawCh:    make(chan rawMsg, 16),
	}
}

// Events 返回事件 channel
func (c *Connection) Events() <-chan Event {
	return c.eventCh
}

// Connect 连接到网关
func (c *Connection) Connect(ctx context.Context) error {
	// 尝试端口 30000 和 80
	var ws *websocket.Conn
	var err error
	for _, port := range []int{30000, 80} {
		url := fmt.Sprintf("ws://%s:%d/centrallinkws/", c.host, port)
		c.logger.Info("connecting", "url", url)
		ws, _, err = websocket.Dial(ctx, url, nil)
		if err == nil {
			break
		}
		c.logger.Debug("dial failed", "port", port, "error", err)
	}
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	c.ws = ws

	go c.readLoop(ctx)

	// 1. 协议协商
	if err := c.sendProtocolList(); err != nil {
		return err
	}
	if err := c.waitForType(ctx, dataTypeSelectedProto, 5*time.Second); err != nil {
		return fmt.Errorf("protocol negotiation: %w", err)
	}
	c.logger.Info("protocol selected: passcode")

	// 2. ECJPAKE 密钥交换
	if err := c.doECJPAKE(ctx); err != nil {
		return fmt.Errorf("ecjpake: %w", err)
	}

	// 3. 等待第一个加密 DATA（网关确认）
	c.recvFirstData(ctx)

	c.logger.Info("secure session established")
	c.sendEvent(Event{Method: "connected"})
	return nil
}

// Call 发送 JSON-RPC 请求
func (c *Connection) Call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	cipher := c.sendCipher
	c.mu.Unlock()
	if cipher == nil {
		return nil, fmt.Errorf("secure session not established")
	}

	id := c.nextReqID()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "/api/" + method,
	}
	if params != nil {
		req["params"] = params
	}

	idStr := fmt.Sprintf("%d", id)
	ch := make(chan json.RawMessage, 1)
	c.pendingMu.Lock()
	c.pending[idStr] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, idStr)
		c.pendingMu.Unlock()
	}()

	if err := c.sendEncrypted(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout (%v)", timeout)
	}
}

// Close 关闭连接
func (c *Connection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.ws != nil {
		c.ws.Close(websocket.StatusNormalClosure, "")
	}
	c.sendEvent(Event{Method: "disconnected"})
}

// === 内部实现 ===

func (c *Connection) nextReqID() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqID++
	return c.reqID
}

func (c *Connection) sendProtocolList() error {
	return c.writeBin(append([]byte{dataTypeProtocolList}, []byte(`["passcode"]`)...))
}

func (c *Connection) doECJPAKE(ctx context.Context) error {
	ec, err := NewECJPAKE("client", c.passcode)
	if err != nil {
		return err
	}

	// R1 发送
	r1, _ := ec.WriteRoundOne()
	if err := c.writeBin(append([]byte{dataTypeECJPAKERoundOne}, r1...)); err != nil {
		return fmt.Errorf("send R1: %w", err)
	}

	// R1 接收
	serverR1, err := c.waitForPayload(ctx, dataTypeECJPAKERoundOne, 5*time.Second)
	if err != nil {
		return fmt.Errorf("recv R1: %w", err)
	}
	if err := ec.ReadRoundOne(serverR1); err != nil {
		return fmt.Errorf("read R1: %w", err)
	}

	// R2 发送
	r2, _ := ec.WriteRoundTwo()
	if err := c.writeBin(append([]byte{dataTypeECJPAKERoundTwo}, r2...)); err != nil {
		return fmt.Errorf("send R2: %w", err)
	}

	// R2 接收
	serverR2, err := c.waitForPayload(ctx, dataTypeECJPAKERoundTwo, 5*time.Second)
	if err != nil {
		return fmt.Errorf("recv R2: %w", err)
	}
	sharedKey, err := ec.ReadRoundTwo(serverR2)
	if err != nil {
		return fmt.Errorf("read R2: %w", err)
	}

	// 密钥派生
	sessionCipher, _ := NewCipher(sharedKey[0:16], sharedKey[16:24])

	// 生成发送密钥
	sendData := make([]byte, 24)
	rand.Read(sendData)
	c.mu.Lock()
	c.sendCipher, _ = NewCipher(sendData[0:16], sendData[16:24])
	c.mu.Unlock()

	// 发送 SESSION_KEY_EXCHANGE
	encrypted, _ := sessionCipher.Encrypt(sendData)
	if err := c.writeBin(append([]byte{dataTypeSessionKeyExch}, encrypted...)); err != nil {
		return fmt.Errorf("send key exch: %w", err)
	}

	// 接收对方的 SESSION_KEY_EXCHANGE
	keyExchData, err := c.waitForPayload(ctx, dataTypeSessionKeyExch, 5*time.Second)
	if err != nil {
		return fmt.Errorf("recv key exch: %w", err)
	}
	recvData, err := sessionCipher.Decrypt(keyExchData)
	if err != nil {
		return fmt.Errorf("decrypt recv key: %w", err)
	}

	c.mu.Lock()
	c.recvCipher, _ = NewCipher(recvData[0:16], recvData[16:24])
	c.mu.Unlock()

	c.logger.Info("AES-GCM channels established")
	return nil
}

func (c *Connection) recvFirstData(ctx context.Context) {
	// 网关可能发送加密数据确认连接，给 3s 超时
	select {
	case evt := <-c.eventCh:
		if evt.Method != "" {
			c.logger.Debug("first data received", "method", evt.Method)
		}
	case <-time.After(3 * time.Second):
		// 超时也 OK，有些网关不发确认
	}
}

// waitForType 等待特定类型的消息
func (c *Connection) waitForType(ctx context.Context, expectedType byte, timeout time.Duration) error {
	select {
	case msg := <-c.rawCh:
		if msg.err != nil {
			return msg.err
		}
		if len(msg.data) < 1 || msg.data[0] != expectedType {
			return fmt.Errorf("expected type %d, got %v", expectedType, msg.data)
		}
		// 处理 SELECTED_PROTOCOL
		if expectedType == dataTypeSelectedProto {
			c.handleSelectedProtocol(msg.data[1:])
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for type %d", expectedType)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForPayload 等待特定类型并返回 payload（去掉类型字节）
func (c *Connection) waitForPayload(ctx context.Context, expectedType byte, timeout time.Duration) ([]byte, error) {
	select {
	case msg := <-c.rawCh:
		if msg.err != nil {
			return nil, msg.err
		}
		if len(msg.data) < 1 || msg.data[0] != expectedType {
			return nil, fmt.Errorf("expected type %d, got %v", expectedType, msg.data[0])
		}
		return msg.data[1:], nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for type %d", expectedType)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readLoop 读取 WebSocket 消息
func (c *Connection) readLoop(ctx context.Context) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if !c.closed {
				c.sendEvent(Event{Method: "disconnected", Error: err.Error()})
			}
			return
		}
		if typ != websocket.MessageBinary || len(data) < 1 {
			continue
		}

		switch data[0] {
		case dataTypeSelectedProto, dataTypeECJPAKERoundOne,
			dataTypeECJPAKERoundTwo, dataTypeSessionKeyExch:
			c.rawCh <- rawMsg{data: data}

		case dataTypeData:
			c.handleEncryptedData(data[1:])

		case dataTypeError:
			c.sendEvent(Event{Method: "disconnected", Error: "gateway error"})

		default:
			c.logger.Debug("unknown msg type", "type", data[0])
		}
	}
}

func (c *Connection) handleSelectedProtocol(payload []byte) {
	c.logger.Info("protocol selected", "payload", string(payload))
	c.sendEvent(Event{Method: "selected_protocol"})
}

func (c *Connection) handleEncryptedData(payload []byte) {
	c.mu.Lock()
	cipher := c.recvCipher
	c.mu.Unlock()
	if cipher == nil {
		return
	}

	decrypted, err := cipher.Decrypt(payload)
	if err != nil {
		c.logger.Error("decrypt", "error", err)
		return
	}

	decompressed, err := decompress(decrypted)
	if err != nil {
		decompressed, err = decompressRaw(decrypted)
		if err != nil {
			c.logger.Error("decompress", "error", err)
			return
		}
	}

	jsonStr := extractFirstJSON(string(decompressed))
	if jsonStr == "" {
		return
	}

	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
		return
	}

	// 响应匹配 pending
	if msg.ID != nil {
		idStr := fmt.Sprintf("%v", msg.ID)
		c.pendingMu.Lock()
		ch, ok := c.pending[idStr]
		if ok {
			delete(c.pending, idStr)
		}
		c.pendingMu.Unlock()
		if ok {
			if msg.Error != nil {
				e, _ := json.Marshal(msg.Error)
				ch <- json.RawMessage(fmt.Sprintf(`{"error":%s}`, e))
			} else {
				ch <- msg.Result
			}
			return
		}
	}

	c.sendEvent(Event{Method: msg.Method, Raw: json.RawMessage(jsonStr)})
}

func (c *Connection) sendEncrypted(rpc any) error {
	data, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	compressed, err := compress(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	cipher := c.sendCipher
	c.mu.Unlock()
	encrypted, err := cipher.Encrypt(compressed)
	if err != nil {
		return err
	}
	return c.writeBin(append([]byte{dataTypeData}, encrypted...))
}

func (c *Connection) writeBin(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ws == nil {
		return fmt.Errorf("connection closed")
	}
	return c.ws.Write(context.Background(), websocket.MessageBinary, data)
}

func (c *Connection) sendEvent(evt Event) {
	select {
	case c.eventCh <- evt:
	default:
	}
}

// === 压缩 ===

func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes(), nil
}

func decompress(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("too short")
	}
	r := flate.NewReader(bytes.NewReader(data[4:]))
	defer r.Close()
	return io.ReadAll(r)
}

func decompressRaw(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	return io.ReadAll(r)
}

func extractFirstJSON(s string) string {
	depth, start := 0, -1
	inStr, esc := false, false
	for i, ch := range s {
		if esc {
			esc = false
			continue
		}
		if ch == '\\' && inStr {
			esc = true
			continue
		}
		if ch == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if ch == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 && start >= 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
