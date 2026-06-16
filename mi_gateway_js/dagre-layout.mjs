#!/usr/bin/env node
/**
 * dagre-layout.mjs — 轻量 dagre 布局引擎
 *
 * 从 stdin 读取 JSON，输出布局后的节点位置。
 * 输入格式：{ nodes: [{id, width, height}], edges: [{from, to}] }
 * 输出格式：{ positions: { id: {x, y, width, height} } }
 *
 * 用法：
 *   echo '{"nodes":[...],"edges":[...]}' | node dagre-layout.mjs
 */

import dagre from 'dagre';

const input = await new Promise((resolve) => {
    let data = '';
    process.stdin.on('data', chunk => data += chunk);
    process.stdin.on('end', () => resolve(data));
});

try {
    const { nodes, edges } = JSON.parse(input);

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

    process.stdout.write(JSON.stringify({ positions }));
} catch (err) {
    process.stdout.write(JSON.stringify({ error: err.message }));
    process.exit(1);
}
