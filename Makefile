# Mi Home Gateway MCP — Go Rewrite
#
# 构建:
#   make build          构建两个二进制
#   make daemon         只构建 mgwd
#   make mcp            只构建 mgw-mcp
#   make install        安装到 ~/.local/bin + systemd
#   make clean          清理

BINARY_DIR := bin
DAEMON_BIN := $(BINARY_DIR)/mgwd
MCP_BIN := $(BINARY_DIR)/mgw-mcp
JS_DIR := mi_gateway_js

GO := go
GOFLAGS := -ldflags="-s -w"

.PHONY: all build daemon mcp install clean test fmt lint

all: build

build: daemon mcp

daemon:
	$(GO) build $(GOFLAGS) -o $(DAEMON_BIN) ./cmd/mgwd/

mcp:
	$(GO) build $(GOFLAGS) -o $(MCP_BIN) ./cmd/mgw-mcp/

install: build
	@echo "Installing binaries to ~/.local/bin..."
	install -d ~/.local/bin
	install -m 755 $(DAEMON_BIN) ~/.local/bin/
	install -m 755 $(MCP_BIN) ~/.local/bin/
	@# JS 文件
	install -d ~/.hermes/mi_gateway_js
	cp -n $(JS_DIR)/daemon.mjs ~/.hermes/mi_gateway_js/ 2>/dev/null || true
	cp -n $(JS_DIR)/gateway.js ~/.hermes/mi_gateway_js/ 2>/dev/null || true
	cp -n $(JS_DIR)/package.json ~/.hermes/mi_gateway_js/ 2>/dev/null || true
	cd ~/.hermes/mi_gateway_js && npm install --production 2>/dev/null || echo "⚠ npm install failed — run manually"
	@# systemd
	install -d ~/.config/systemd/user
	sed -e "s|__JS_DIR__|$$HOME/.hermes/mi_gateway_js|g" \
	    deploy/mgwd.service > ~/.config/systemd/user/mgwd.service
	@echo ""
	@echo "✅ Installed. Usage:"
	@echo "  mgwd --host 192.168.1.x"
	@echo "  systemctl --user enable --now mgwd"

clean:
	rm -rf $(BINARY_DIR)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...
