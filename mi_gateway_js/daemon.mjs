#!/usr/bin/env node
/**
 * mi-gateway-daemon.mjs — Persistent WebSocket connection to Xiaomi Central Hub Gateway
 *
 * MODES:
 *   --tcp         Listen on TCP port for JSON-RPC (default: 19345)
 *   (no flag)     stdin/stdout JSON-RPC (original mode, backward compatible)
 *   --proxy       Start Web UI proxy (experimental)
 *
 * CLI ARGS:
 *   --host <IP>       Gateway IP address (overrides MGW_HOST env)
 *   --passcode <6dig> 6-digit dynamic password (overrides MGW_PASSCODE env)
 *   --port <num>      TCP port for --tcp mode (default: 19345)
 *
 * Config sources (host, checked in order):
 *   1. --host CLI arg
 *   2. MGW_HOST env var
 *   3. ~/.hermes/mihome/host file (written by set_host RPC)
 *
 * Passcode sources (checked in order):
 *   1. --passcode CLI arg
 *   2. MGW_PASSCODE env var (on startup)
 *   3. ~/.hermes/mihome/passcode file (persistent, polled every 2s)
 *   4. set_passcode JSON-RPC (runtime update via stdin or TCP)
 *
 * RUNTIME CONFIG (via JSON-RPC):
 *   set_passcode({passcode})    — Update passcode and reconnect
 *   set_host({host})            — Update gateway IP and reconnect
 *   get_config                  — Show current host/passcode/connection config
 *   ping                        — Check connection status
 *
 * JSON-RPC over stdin/stdout OR TCP.
 * NEVER exits unless stdin closes, SIGTERM, or no clients in TCP-only mode.
 * Auto-reconnects on disconnect with exponential backoff (3s→30s max).
 */

import { Gateway } from './gateway.js';
import fs from 'fs';
import path from 'path';
import os from 'os';
import { createServer } from 'net';

const HERMES_HOME = process.env.HERMES_HOME || path.join(os.homedir(), '.hermes');
const PASSCODE_DIR = path.join(HERMES_HOME, 'mihome');
const PASSCODE_FILE = path.join(PASSCODE_DIR, 'passcode');
const CONNECT_TIMEOUT = 25000;
const RECONNECT_MIN_DELAY = 3000;
const RECONNECT_MAX_DELAY = 30000;

// CLI flags
const USE_TCP = process.argv.includes('--tcp');
const USE_STDIN = !USE_TCP;  // Default: stdin mode (backward compat)
const USE_PROXY = process.argv.includes('--proxy');

// Parse CLI args — override env/file but don't hardcode defaults
function getArg(flag, defaultVal = null) {
    const idx = process.argv.indexOf(flag);
    return idx >= 0 && idx + 1 < process.argv.length ? process.argv[idx + 1] : defaultVal;
}
const CLI_HOST = getArg('--host');
const CLI_PASSCODE = getArg('--passcode');
const CLI_PORT = getArg('--port');

// Read host from persistent file (written by set_host RPC or MCP tool)
function readHostFile() {
    try {
        const f = path.join(PASSCODE_DIR, 'host');
        if (fs.existsSync(f)) {
            const c = fs.readFileSync(f, 'utf8').trim();
            if (c) return c;
        }
    } catch (_) {}
    return null;
}

let HOST = CLI_HOST || process.env.MGW_HOST || readHostFile() || '';
const TCP_PORT = parseInt(CLI_PORT || process.env.MGW_TCP_PORT || '19345', 10);

let gateway = null;
let reqId = 0;
let pending = new Map();       // requestId → { resolve, reject, timeout }
let connected = false;
let passcode = CLI_PASSCODE || process.env.MGW_PASSCODE || '';
let connecting = false;
let shuttingDown = false;
let reconnectAttempts = 0;
let authFailed = false;
let lastPasscodeSaved = null;  // Used by gateway.js
const MAX_RECONNECT_ATTEMPTS = 5;

// ======== Multi-client output ========
let tcpSockets = new Set();    // all connected TCP sockets

function sendToStdout(obj) {
    const line = JSON.stringify(obj) + '\n';
    process.stdout.write(line);
}

function sendToTcpClients(obj) {
    const line = JSON.stringify(obj) + '\n';
    const data = Buffer.from(line);
    for (const sock of tcpSockets) {
        try { sock.write(data); } catch (_) { /* socket closed */ }
    }
}

function sendAll(obj) {
    if (USE_STDIN) sendToStdout(obj);
    if (tcpSockets.size > 0) sendToTcpClients(obj);
}

// Ensure passcode directory exists
try { fs.mkdirSync(PASSCODE_DIR, { recursive: true }); } catch (_) {}

// ======== Read passcode from file ========
function readPasscodeFile() {
    try {
        if (fs.existsSync(PASSCODE_FILE)) {
            return fs.readFileSync(PASSCODE_FILE, 'utf8').trim();
        }
    } catch (_) {}
    return null;
}

function checkPasscodeFile() {
    const c = readPasscodeFile();
    if (c && c !== passcode) {
        passcode = c;
        return true;
    }
    return false;
}

// Poll passcode file for updates — reconnect when changed
setInterval(() => {
    if (shuttingDown) return;
    if (checkPasscodeFile()) {
        sendAll({ method: 'passcode_updated' });
        if (!connected && !connecting) {
            reconnectAttempts = 0;
            connect();
        } else if (connected) {
            closeGateway();
            connected = false;
            connecting = false;
            reconnectAttempts = 0;
            connect();
        }
    }
}, 2000);

// ======== Helper: safely close gateway connection ========
function closeGateway() {
    if (gateway) {
        try { gateway.close(); } catch (_) {}
        gateway = null;
    }
}

// ======== Connect ========
function connect() {
    if (connecting || shuttingDown) return;
    if (!passcode) return;
    if (!HOST) {
        sendAll({ method: 'status', message: 'Gateway IP not configured. Set MGW_HOST env var, or use set_host RPC.' });
        return;
    }

    connecting = true;
    authFailed = false;
    sendAll({ method: 'connecting' });
    reconnectAttempts++;

    const timeout = setTimeout(() => {
        connecting = false;
        sendAll({ method: 'disconnected', error: 'Connection timeout' });
        scheduleReconnect();
    }, CONNECT_TIMEOUT);

    gateway = new Gateway(
        { host: HOST, protocols: ['passcode'] },
        function () { this.setPasscode(passcode); },
        function () {
            connected = true;
            connecting = false;
            reconnectAttempts = 0;
            clearTimeout(timeout);
            sendAll({ method: 'connected' });
        },
        function () {
            connected = false;
            connecting = false;
            authFailed = true;
            clearTimeout(timeout);
            sendAll({ method: 'disconnected', error: 'Authentication failed — passcode expired, use set_passcode to set a new one' });
            reconnectAttempts = 0;
        }
    );

    gateway.onClose = () => {
        connected = false;
        connecting = false;
        if (!shuttingDown) {
            if (authFailed || reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
                const reason = authFailed
                    ? 'Authentication failed — passcode expired'
                    : `Reconnection failed after ${MAX_RECONNECT_ATTEMPTS} attempts — passcode may have expired`;
                sendAll({ method: 'disconnected', error: `${reason}, use set_passcode to set a new one` });
                reconnectAttempts = 0;
                authFailed = false;
            } else {
                sendAll({ method: 'disconnected', error: 'Connection closed — reconnecting' });
                scheduleReconnect();
            }
        }
    };
}

// ======== Exponential Backoff Reconnect ========
let reconnectTimer = null;
function scheduleReconnect() {
    if (reconnectTimer || shuttingDown) return;
    const delay = Math.min(
        RECONNECT_MIN_DELAY * Math.pow(2, reconnectAttempts - 1),
        RECONNECT_MAX_DELAY
    );
    reconnectTimer = setTimeout(() => {
        reconnectTimer = null;
        if (!connected && !connecting && !shuttingDown) connect();
    }, delay);
}

// ======== Write passcode to persistent file ========
function savePasscode(pc) {
    passcode = pc;
    lastPasscodeSaved = pc;
    try {
        fs.writeFileSync(PASSCODE_FILE, pc, 'utf8');
        sendAll({ method: 'passcode_saved', path: PASSCODE_FILE });
    } catch (e) {
        sendAll({ method: 'passcode_save_error', error: e.message });
    }
}

// ======== Handle API calls ========
async function handleRequest(req, sourceSocket = null) {
    const { id, method, params } = req;

    if (method === 'set_passcode') {
        savePasscode(params?.passcode || '');
        authFailed = false;
        reconnectAttempts = 0;
        closeGateway();
        connecting = false;
        connected = false;
        if (id) sendResponse(id, { status: 'passcode_set' }, sourceSocket);
        // Trigger connect immediately — don't wait for 2s poll interval
        if (passcode) connect();
        return;
    }

    if (method === 'set_host') {
        // Update HOST at runtime — reconnect with new address
        const newHost = params?.host || '';
        if (!newHost) {
            if (id) sendResponse(id, { error: 'host is required' }, sourceSocket);
            return;
        }
        HOST = newHost;
        process.env.MGW_HOST = newHost;
        try {
            fs.writeFileSync(path.join(PASSCODE_DIR, 'host'), newHost, 'utf8');
        } catch (e) {
            // non-fatal
        }
        authFailed = false;
        reconnectAttempts = 0;
        closeGateway();
        connecting = false;
        connected = false;
        if (id) sendResponse(id, { status: 'host_set', host: newHost }, sourceSocket);
        if (passcode) connect();
        return;
    }

    if (method === 'get_session_keys') {
        if (!connected || !gateway) {
            if (id) sendResponse(id, { error: 'Not connected' }, sourceSocket);
            return;
        }
        const keys = {
            sessionKey: Array.from(gateway._sessionKey || []).map(b => b.toString(16).padStart(2,'0')).join(''),
            sessionSalt: Array.from(gateway._sessionSalt || []).map(b => b.toString(16).padStart(2,'0')).join(''),
            sendKey: Array.from(gateway._sendKey || []).map(b => b.toString(16).padStart(2,'0')).join(''),
            sendSalt: Array.from(gateway._sendSalt || []).map(b => b.toString(16).padStart(2,'0')).join(''),
        };
        if (id) sendResponse(id, keys, sourceSocket);
        return;
    }

    if (method === 'ping') {
        if (id) sendResponse(id, { pong: true, connected, passcode_set: !!passcode, host: HOST }, sourceSocket);
        return;
    }

    if (method === 'get_config') {
        if (id) sendResponse(id, {
            host: HOST,
            passcode_set: !!passcode,
            connected,
            tcp_port: TCP_PORT,
            tcp_mode: USE_TCP,
            config_files: {
                passcode: PASSCODE_FILE,
                host: path.join(PASSCODE_DIR, 'host'),
            }
        }, sourceSocket);
        return;
    }

    if (method === 'dagre_layout') {
        // Pure computation — no gateway needed
        const { nodes, edges } = params || {};
        if (!nodes || !Array.isArray(nodes)) {
            if (id) sendResponse(id, { error: 'nodes array required' }, sourceSocket);
            return;
        }
        try {
            const dagreMod = await import('dagre');
            const dagre = dagreMod.default || dagreMod;
            const g = new dagre.graphlib.Graph();
            g.setGraph({ rankdir: 'LR', nodesep: 40, ranksep: 50, marginx: 30, marginy: 30 });
            g.setDefaultEdgeLabel(() => ({}));
            for (const n of nodes) {
                g.setNode(n.id, { width: n.width || 160, height: n.height || 98 });
            }
            for (const e of (edges || [])) {
                g.setEdge(e.from, e.to);
            }
            dagre.layout(g);
            const positions = {};
            for (const n of nodes) {
                const p = g.node(n.id);
                if (p) {
                    positions[n.id] = {
                        x: Math.round(p.x - p.width / 2),
                        y: Math.round(p.y - p.height / 2),
                        width: p.width,
                        height: p.height,
                    };
                }
            }
            if (id) sendResponse(id, { positions }, sourceSocket);
        } catch (err) {
            if (id) sendResponse(id, { error: err.message }, sourceSocket);
        }
        return;
    }

    if (!id) return;
    if (!connected) {
        sendResponse(id, { error: 'Not connected. Use set_passcode first.' }, sourceSocket);
        return;
    }

    try {
        let result;
        switch (method) {
            case 'auth':
                try { result = await gateway.callAPI('getVarList', {scope:'global'}, 5000); }
                catch(_) { result = { status: 'connected' }; }
                break;
            case 'devices': case 'list_devices':
                result = await gateway.callAPI('getDevList', {}, 15000);
                break;
            case 'scenes': case 'list_scenes':
                result = await gateway.callAPI('getGraphList', {}, 15000);
                break;
            case 'get_graph':
                const gid = params?.graphId || params?.id || params?.graph_id || '';
                result = await gateway.callAPI('getGraph', { id: gid }, 15000);
                break;
            case 'get_graph_list':
                result = await gateway.callAPI('getGraphList', {}, 15000);
                break;
            case 'delete_graph':
                result = await gateway.callAPI('deleteGraph', params || {}, 10000);
                break;
            case 'change_graph_config':
                result = await gateway.callAPI('changeGraphConfig', params || {}, 10000);
                break;
            case 'execute_scene':
                result = await gateway.callAPI('changeGraphConfig', { graphId: params?.scene_id, config: { start: params?.start ?? true } }, 10000);
                break;
            case 'get_vars':
                result = await gateway.callAPI('getVarList', { scope: params?.scope || 'global' }, 5000);
                break;
            case 'set_var':
                result = await gateway.callAPI('setVarValue', { scope: params?.scope || 'global', id: params?.name, value: params?.value }, 5000);
                break;
            case 'set_graph':
                result = await gateway.callAPI('setGraph', params || {}, 10000);
                break;
            case 'device_specs_extra':
                const dl = await gateway.callAPI('getDevList', {}, 15000);
                const ds = dl?.devList || dl || {};
                const enr = {};
                for (const [d, dev] of Object.entries(ds)) enr[d] = { ...dev };
                result = enr;
                break;
            // === Backup management ===
            case 'get_backup_list':
                result = await gateway.callAPI('getBackupList', { from: 'fds' }, 15000);
                break;
            case 'create_backup':
                const createParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('createBackup', createParams, 30000);
                break;
            case 'generate_backup':
                const genParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('generateBackup', genParams, 30000);
                break;
            case 'download_backup':
                const dlParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('downloadBackup', dlParams, 15000);
                break;
            case 'load_backup':
                result = await gateway.callAPI('loadBackup', params?.params || params || {}, 30000);
                break;
            case 'delete_backup':
                const delParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('deleteBackup', delParams, 10000);
                break;
            case 'get_backup_progress':
                const progParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('getBackupProgress', progParams, 15000);
                break;
            case 'get_backup_config':
                result = await gateway.callAPI('getBackupConfig', { from: 'fds' }, 15000);
                break;
            case 'set_backup_config':
                const setCfgParams = { from: 'fds', params: params?.params || params || {} };
                result = await gateway.callAPI('setBackupConfig', setCfgParams, 10000);
                break;
            // === Logs ===
            case 'get_log':
                result = await gateway.callAPI('getLog', params || {}, 15000);
                break;
            // === Variable advanced CRUD ===
            case 'create_var':
                result = await gateway.callAPI('createVar', params || {}, 5000);
                break;
            case 'delete_var':
                result = await gateway.callAPI('deleteVar', params || {}, 5000);
                break;
            case 'get_var_config':
                result = await gateway.callAPI('getVarConfig', params || {}, 5000);
                break;
            case 'set_var_config':
                result = await gateway.callAPI('setVarConfig', params || {}, 5000);
                break;
            case 'get_var_value':
                result = await gateway.callAPI('getVarValue', params || {}, 5000);
                break;
            case 'get_var_scope_list':
                result = await gateway.callAPI('getVarScopeList', {}, 5000);
                break;
            default:
                result = await gateway.callAPI(method, params || {}, 10000);
        }
        sendResponse(id, result, sourceSocket);
    } catch (e) {
        sendResponse(id, { error: e.message }, sourceSocket);
    }
}

function sendResponse(id, result, sourceSocket) {
    const obj = typeof id === 'object' ? id : { id, result };
    if (sourceSocket) {
        try { sourceSocket.write(JSON.stringify(obj) + '\n'); } catch(_) {}
    } else {
        sendToStdout(obj);
    }
}

// ======== TCP Server ========
function startTcpServer() {
    const server = createServer(socket => {
        socket.id = `${Date.now()}_${Math.random().toString(36).slice(2,8)}`;
        tcpSockets.add(socket);
        let buf = '';

        const MAX_BUF_SIZE = 1024 * 1024;  // 1MB max buffer per socket

        socket.on('data', chunk => {
            if (buf.length + chunk.length > MAX_BUF_SIZE) {
                buf = '';
                socket.destroy();
                return;
            }
            buf += chunk.toString();
            const lines = buf.split('\n');
            buf = lines.pop();
            for (const line of lines) {
                if (!line.trim()) continue;
                try { handleRequest(JSON.parse(line), socket); }
                catch (e) { /* skip invalid JSON */ }
            }
        });

        socket.on('close', () => {
            tcpSockets.delete(socket);
            process.stderr.write(`[TCP] client ${socket.id} disconnected, remaining: ${tcpSockets.size}\n`);
            // In TCP-only mode, stay alive even with 0 clients
        });

        socket.on('error', () => {
            tcpSockets.delete(socket);
            process.stderr.write(`[TCP] client ${socket.id} error\n`);
        });
    });

    server.listen(TCP_PORT, '127.0.0.1', () => {
        process.stderr.write(`[TCP] listening on 127.0.0.1:${TCP_PORT}\n`);
        sendToStdout(JSON.stringify({ method: 'tcp_listening', port: TCP_PORT }) + '\n');
    });

    server.on('error', err => {
        sendToStdout(JSON.stringify({ method: 'tcp_error', error: err.message }) + '\n');
    });

    return server;
}

// ======== Stdin JSON-RPC reader (stdin mode only) ========
if (USE_STDIN) {
    let buf = '';
    process.stdin.on('data', chunk => {
        buf += chunk.toString();
        const lines = buf.split('\n');
        buf = lines.pop();
        for (const line of lines) {
            if (!line.trim()) continue;
            try { handleRequest(JSON.parse(line)); }
            catch (e) { /* skip invalid JSON */ }
        }
    });

    process.stdin.on('end', () => {
        shuttingDown = true;
        if (gateway) gateway.close();
        process.exit(0);
    });
}

// Handle signals gracefully
process.on('SIGTERM', () => {
    shuttingDown = true;
    if (gateway) gateway.close();
    process.exit(0);
});
process.on('SIGINT', () => {
    shuttingDown = true;
    if (gateway) gateway.close();
    process.exit(0);
});

// Catch unhandled rejections so they don't crash the daemon
process.on('unhandledRejection', (reason) => {
    sendAll({ method: 'status', message: `Warning: unhandled rejection: ${String(reason).slice(0, 100)}` });
});
process.on('uncaughtException', (err) => {
    sendAll({ method: 'status', message: `Warning: uncaught exception: ${String(err).slice(0, 100)}` });
});

// ======== Keepalive ========
let keepaliveFailures = 0;
const KEEPALIVE_MAX_FAILURES = 3;

setInterval(() => {
    if (connected && gateway) {
        gateway.callAPI('getGraphList', {}, 5000)
            .then(() => { keepaliveFailures = 0; })
            .catch(() => {
                keepaliveFailures++;
                if (keepaliveFailures >= KEEPALIVE_MAX_FAILURES) {
                    keepaliveFailures = 0;
                    closeGateway();
                }
            });
    }
}, 25000);

// ======== Startup ========
sendToStdout(JSON.stringify({ method: 'starting' }) + '\n');

if (USE_TCP) {
    startTcpServer();
}

// Start Web UI proxy if requested
if (USE_PROXY) {
    import('./proxy.mjs').then(mod => {
        mod.init(handleRequest);
    }).catch(e => {
        sendToStdout(JSON.stringify({ method: 'proxy_error', error: e.message }) + '\n');
    });
}

checkPasscodeFile();
if (passcode) {
    sendToStdout(JSON.stringify({ method: 'status', message: 'Passcode available via env/file, connecting...' }) + '\n');
    connect();
} else {
    sendToStdout(JSON.stringify({ method: 'status', message: 'No passcode found. Use set_passcode to connect.' }) + '\n');
}
