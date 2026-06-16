// Package native — 场景图自动布局
//
// 优先通过 subprocess 调用 dagre (Node.js)，fallback 到本地 Sugiyama 算法。
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 卡片尺寸表
var cardSizes = map[string][2]int{
	"onLoad":            {160, 98},
	"loop":              {160, 98},
	"alarmClock":        {120, 60},
	"nop":               {120, 60},
	"deviceInput":       {280, 120},
	"deviceGet":         {280, 120},
	"deviceGetSetVar":   {280, 120},
	"deviceInputSetVar": {280, 120},
	"deviceOutput":      {280, 120},
	"logicOr":           {180, 80},
	"logicAnd":          {180, 80},
	"logicNot":          {140, 60},
	"delay":             {180, 80},
	"statusLast":        {180, 80},
	"register":          {180, 120},
	"condition":         {260, 120},
	"signalOr":          {180, 80},
	"timeRange":         {180, 80},
	"counter":           {180, 80},
	"modeSwitch":        {180, 80},
	"eventSequence":     {180, 80},
	"onlyNTimes":        {180, 80},
	"varGet":            {180, 80},
	"varChange":         {180, 80},
	"varSetNumber":      {180, 80},
	"varSetString":      {180, 80},
}

var typeCol = map[string]int{
	"onLoad": 0, "loop": 0, "alarmClock": 0, "nop": 0,
	"deviceInput": 1, "deviceGet": 1, "deviceGetSetVar": 1, "deviceInputSetVar": 1,
	"signalOr": 2, "logicOr": 2, "logicNot": 2,
	"register": 2, "timeRange": 2, "counter": 2, "modeSwitch": 2,
	"eventSequence": 2, "onlyNTimes": 2,
	"varGet": 2, "varChange": 2, "varSetNumber": 2, "varSetString": 2,
	"logicAnd": 3, "delay": 3,
	"deviceOutput": 4,
	"condition":    5,
	"statusLast":   6,
}

// GetCardDimensions 获取卡片尺寸
func GetCardDimensions(nodeType string) (int, int) {
	if s, ok := cardSizes[nodeType]; ok {
		return s[0], s[1]
	}
	return 280, 120
}

// SceneNode 场景节点
type SceneNode struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Cfg     map[string]interface{} `json:"cfg"`
	Inputs  map[string]interface{} `json:"inputs"`
	Outputs map[string][]string    `json:"outputs"`
}

// LayoutConfig 布局配置
type LayoutConfig struct {
	JsDir string    // dagre-layout.mjs 所在目录
	Node  string    // node 二进制路径
	Log   *slog.Logger
}

// LayoutNodes 自动布局场景节点
// 优先用 dagre，fallback 到本地 Sugiyama
func LayoutNodes(nodes []*SceneNode, cfg LayoutConfig) {
	if len(nodes) == 0 {
		return
	}

	// 尝试 dagre
	if cfg.JsDir != "" {
		if err := layoutDagre(nodes, cfg); err == nil {
			return
		}
		if cfg.Log != nil {
			cfg.Log.Debug("dagre layout failed, falling back to local algorithm")
		}
	}

	// fallback: 本地 Sugiyama
	layoutSugiyama(nodes)
}

// === dagre 布局（通过 subprocess）===

type dagreInput struct {
	Nodes []dagreNode `json:"nodes"`
	Edges []dagreEdge `json:"edges"`
}

type dagreNode struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type dagreEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type dagreOutput struct {
	Positions map[string]dagrePos `json:"positions"`
	Error     string              `json:"error"`
}

type dagrePos struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func layoutDagre(nodes []*SceneNode, cfg LayoutConfig) error {
	// 构建 dagre 输入
	input := dagreInput{}
	nodeSet := make(map[string]bool)
	for _, n := range nodes {
		nodeSet[n.ID] = true
		w, h := GetCardDimensions(n.Type)
		// 从 cfg.pos 获取实际尺寸（如果有）
		if pos, ok := n.Cfg["pos"].(map[string]interface{}); ok {
			if pw, ok := pos["width"].(float64); ok && pw > 0 {
				w = int(pw)
			}
			if ph, ok := pos["height"].(float64); ok && ph > 0 {
				h = int(ph)
			}
		}
		input.Nodes = append(input.Nodes, dagreNode{ID: n.ID, Width: w, Height: h})
	}

	// 提取边
	for _, n := range nodes {
		for _, targets := range n.Outputs {
			for _, t := range targets {
				tid := t
				if idx := strings.Index(t, "."); idx >= 0 {
					tid = t[:idx]
				}
				if nodeSet[tid] {
					input.Edges = append(input.Edges, dagreEdge{From: n.ID, To: tid})
				}
			}
		}
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return err
	}

	// 找 node 二进制
	nodeBin := cfg.Node
	if nodeBin == "" {
		nodeBin, err = exec.LookPath("node")
		if err != nil {
			return fmt.Errorf("node not found: %w", err)
		}
	}

	scriptPath := filepath.Join(cfg.JsDir, "dagre-layout.mjs")

	// 调用 subprocess
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(inputJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dagre subprocess: %w, stderr: %s", err, stderr.String())
	}

	// 解析输出
	var output dagreOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return fmt.Errorf("parse dagre output: %w", err)
	}
	if output.Error != "" {
		return fmt.Errorf("dagre error: %s", output.Error)
	}

	// 应用位置
	for _, n := range nodes {
		if pos, ok := output.Positions[n.ID]; ok {
			if n.Cfg == nil {
				n.Cfg = make(map[string]interface{})
			}
			n.Cfg["pos"] = map[string]interface{}{
				"x":      pos.X,
				"y":      pos.Y,
				"width":  pos.Width,
				"height": pos.Height,
			}
		}
	}

	return nil
}

// === 本地 Sugiyama 布局（fallback）===

func layoutSugiyama(nodes []*SceneNode) {
	forward, backward := buildTopology(nodes)
	layer := assignLayers(nodes, backward)
	colAssign := assignColumns(nodes, layer, forward, backward)

	sizes := make([][2]int, len(nodes))
	for i, n := range nodes {
		w, h := GetCardDimensions(n.Type)
		sizes[i] = [2]int{w, h}
	}
	maxW, maxH, sumH := 0, 0, 0
	for _, s := range sizes {
		if s[0] > maxW {
			maxW = s[0]
		}
		if s[1] > maxH {
			maxH = s[1]
		}
		sumH += s[1]
	}
	avgH := float64(sumH) / float64(len(sizes))

	gapX := math.Max(30, float64(maxW)*0.15)
	gapY := math.Max(20, avgH*0.2)
	cellPad := math.Max(20, float64(maxW)*0.1)
	cellW := float64(maxW) + gapX
	cellH := float64(maxH) + gapY

	for _, n := range nodes {
		w, h := GetCardDimensions(n.Type)
		lv := layer[n.ID]
		cl := colAssign[n.ID]

		x := cellPad + float64(cl)*cellW + (cellW-float64(w))/2
		y := cellPad + float64(lv)*cellH + (cellH-float64(h))/2

		if n.Cfg == nil {
			n.Cfg = make(map[string]interface{})
		}
		n.Cfg["pos"] = map[string]interface{}{
			"x": int(x), "y": int(y), "width": w, "height": h,
		}
	}
}

func buildTopology(nodes []*SceneNode) (forward, backward map[string][]string) {
	nodeSet := make(map[string]bool)
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	forward = make(map[string][]string)
	backward = make(map[string][]string)
	for _, n := range nodes {
		for _, targets := range n.Outputs {
			for _, t := range targets {
				tid := t
				if idx := strings.Index(t, "."); idx >= 0 {
					tid = t[:idx]
				}
				if nodeSet[tid] {
					forward[n.ID] = append(forward[n.ID], tid)
					backward[tid] = append(backward[tid], n.ID)
				}
			}
		}
	}
	return
}

func assignLayers(nodes []*SceneNode, backward map[string][]string) map[string]int {
	forward := make(map[string][]string)
	nodeSet := make(map[string]bool)
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	for _, n := range nodes {
		for _, targets := range n.Outputs {
			for _, t := range targets {
				tid := t
				if idx := strings.Index(t, "."); idx >= 0 {
					tid = t[:idx]
				}
				if nodeSet[tid] {
					forward[n.ID] = append(forward[n.ID], tid)
				}
			}
		}
	}

	layer := make(map[string]int)
	visited := make(map[string]bool)
	queue := []string{}

	for _, n := range nodes {
		if len(backward[n.ID]) == 0 {
			layer[n.ID] = 0
			queue = append(queue, n.ID)
			visited[n.ID] = true
		}
	}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, next := range forward[curr] {
			newL := layer[curr] + 1
			if visited[next] {
				if layer[next] < newL {
					layer[next] = newL
				}
				continue
			}
			visited[next] = true
			layer[next] = newL
			queue = append(queue, next)
		}
	}

	for _, n := range nodes {
		if _, ok := layer[n.ID]; !ok {
			layer[n.ID] = 0
		}
	}
	return layer
}

func assignColumns(nodes []*SceneNode, layer map[string]int, forward, backward map[string][]string) map[string]int {
	byLayer := make(map[int][]*SceneNode)
	for _, n := range nodes {
		byLayer[layer[n.ID]] = append(byLayer[layer[n.ID]], n)
	}

	colAssign := make(map[string]int)
	occupied := make(map[int]map[int]bool)

	maxLayer := 0
	for _, lv := range layer {
		if lv > maxLayer {
			maxLayer = lv
		}
	}

	for lv := 0; lv <= maxLayer; lv++ {
		layerNodes := byLayer[lv]
		if len(layerNodes) == 0 {
			continue
		}
		if occupied[lv] == nil {
			occupied[lv] = make(map[int]bool)
		}

		type desired struct {
			nid string
			col int
		}
		var desiredList []desired

		for _, n := range layerNodes {
			d := typeCol[n.Type]
			preds := backward[n.ID]
			if len(preds) > 0 {
				predCols := []int{}
				for _, p := range preds {
					if c, ok := colAssign[p]; ok {
						predCols = append(predCols, c)
					}
				}
				if len(predCols) > 0 {
					sort.Ints(predCols)
					d = predCols[len(predCols)/2]
				}
			}
			desiredList = append(desiredList, desired{n.ID, d})
		}

		sort.Slice(desiredList, func(i, j int) bool {
			return desiredList[i].col < desiredList[j].col
		})

		for _, dd := range desiredList {
			c := nextFree(lv, dd.col, occupied)
			colAssign[dd.nid] = c
			occupied[lv][c] = true
		}
	}
	return colAssign
}

func nextFree(layer, desired int, occupied map[int]map[int]bool) int {
	col := desired
	if col < 0 {
		col = 0
	}
	for i := 0; i < 50; i++ {
		if !occupied[layer][col] {
			return col
		}
		col++
	}
	return col
}
