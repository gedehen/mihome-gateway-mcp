// Package native — 场景图自动布局算法
//
// Sugiyama 分层布局：bfs 分层 + barycenter 列分配 + 坐标计算
// 对标 Python auto_layout.py 的本地 fallback 算法
package native

import (
	"math"
	"sort"
	"strings"
)

// 卡片尺寸表（对标 Python SIZE）
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

// 默认列顺序（对标 Python TYPE_COL）
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

// SceneNode 场景节点（简化版，用于布局计算）
type SceneNode struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Cfg     map[string]interface{} `json:"cfg"`
	Inputs  map[string]interface{} `json:"inputs"`
	Outputs map[string][]string    `json:"outputs"`
}

// LayoutNodes 自动布局场景节点（原地修改 cfg.pos）
func LayoutNodes(nodes []*SceneNode) {
	if len(nodes) == 0 {
		return
	}

	// 构建拓扑
	forward, backward := buildTopology(nodes)

	// BFS 分层
	layer := assignLayers(nodes, backward)

	// barycenter 列分配
	colAssign := assignColumns(nodes, layer, forward, backward)

	// 动态间距
	sizes := make([][2]int, len(nodes))
	for i, n := range nodes {
		w, h := GetCardDimensions(n.Type); sizes[i] = [2]int{w, h}
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

	// 计算坐标
	for _, n := range nodes {
		w, h := GetCardDimensions(n.Type)
		lv := layer[n.ID]
		cl := colAssign[n.ID]

		cellX := cellPad + float64(cl)*cellW
		x := cellX + (cellW-float64(w))/2

		cellY := cellPad + float64(lv)*cellH
		y := cellY + (cellH-float64(h))/2

		// 写入 cfg.pos
		if n.Cfg == nil {
			n.Cfg = make(map[string]interface{})
		}
		pos := map[string]interface{}{
			"x":      int(x),
			"y":      int(y),
			"width":  w,
			"height": h,
		}
		n.Cfg["pos"] = pos
	}
}

// buildTopology 构建前驱/后继关系
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

// assignLayers BFS 分层：无入边 = layer0, 下游递增
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

	// 无入边的节点 = layer 0
	for _, n := range nodes {
		if len(backward[n.ID]) == 0 {
			layer[n.ID] = 0
			queue = append(queue, n.ID)
			visited[n.ID] = true
		}
	}

	// BFS
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

	// 未访问的节点（环）设为 layer 0
	for _, n := range nodes {
		if _, ok := layer[n.ID]; !ok {
			layer[n.ID] = 0
		}
	}
	return layer
}

// assignColumns barycenter 启发式列分配
func assignColumns(nodes []*SceneNode, layer map[string]int, forward, backward map[string][]string) map[string]int {
	// 按层分组
	byLayer := make(map[int][]*SceneNode)
	for _, n := range nodes {
		lv := layer[n.ID]
		byLayer[lv] = append(byLayer[lv], n)
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

		// 计算期望列
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
					d = predCols[len(predCols)/2] // median
				}
			}
			desiredList = append(desiredList, desired{n.ID, d})
		}

		// 按期望列排序
		sort.Slice(desiredList, func(i, j int) bool {
			return desiredList[i].col < desiredList[j].col
		})

		// 分配列
		for _, dd := range desiredList {
			c := nextFree(lv, dd.col, occupied)
			colAssign[dd.nid] = c
			occupied[lv][c] = true
		}
	}
	return colAssign
}

// nextFree 从左到右找最近的空闲列
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
	// 极端情况
	return col
}
