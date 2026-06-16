// Package native — 场景图自动布局（纯 Go，零外部依赖）
//
// 改进的 Sugiyama 分层布局算法，对标 dagre 效果：
// - 多轮 barycenter 迭代（前向+后向，4-8 轮）
// - 交叉计数 + 保留最优解
// - 节点居中对齐
package native

import (
	"fmt"
	"math"
	"sort"
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

// 默认列优先级
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

// LayoutNodes 自动布局场景节点（纯 Go，无外部依赖）
func LayoutNodes(nodes []*SceneNode) {
	if len(nodes) == 0 {
		return
	}

	// 1. 构建拓扑
	forward, backward := buildTopology(nodes)
	nodeByID := make(map[string]*SceneNode)
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}

	// 2. 分层
	layer := assignLayers(nodes, backward)

	// 3. 多轮 barycenter 迭代，保留最优解
	bestOrder := make(map[int][]string)
	bestCrossings := math.MaxInt32

	// 收集所有层
	layerSet := make(map[int]bool)
	for _, l := range layer {
		layerSet[l] = true
	}
	maxLayer := 0
	for l := range layerSet {
		if l > maxLayer {
			maxLayer = l
		}
	}

	// 按层分组
	byLayer := make(map[int][]string)
	for _, n := range nodes {
		lv := layer[n.ID]
		byLayer[lv] = append(byLayer[lv], n.ID)
	}

	// 初始顺序：按 typeCol 排序
	for lv := 0; lv <= maxLayer; lv++ {
		sort.Slice(byLayer[lv], func(i, j int) bool {
			ni, nj := nodeByID[byLayer[lv][i]], nodeByID[byLayer[lv][j]]
			return typeCol[ni.Type] < typeCol[nj.Type]
		})
	}

	// 多轮迭代（前向+后向，共 8 轮）
	for iter := 0; iter < 8; iter++ {
		if iter%2 == 0 {
			// 前向：从上到下调整
			for lv := 1; lv <= maxLayer; lv++ {
				barycenterSort(byLayer[lv], byLayer[lv-1], forward, backward, nodeByID)
			}
		} else {
			// 后向：从下到上调整
			for lv := maxLayer - 1; lv >= 0; lv-- {
				barycenterSort(byLayer[lv], byLayer[lv+1], backward, forward, nodeByID)
			}
		}

		// 计算交叉数
		crossings := countCrossings(byLayer, forward, nodeByID)
		if crossings < bestCrossings {
			bestCrossings = crossings
			for lv, ids := range byLayer {
				bestOrder[lv] = make([]string, len(ids))
				copy(bestOrder[lv], ids)
			}
		}

		// 无交叉则提前退出
		if crossings == 0 {
			break
		}
	}

	// 恢复最优解
	if bestCrossings < math.MaxInt32 {
		for lv, ids := range bestOrder {
			byLayer[lv] = ids
		}
	}

	// 4. 计算坐标
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

	gapX := math.Max(40, float64(maxW)*0.2)  // 列间距更大
	gapY := math.Max(30, avgH*0.25)          // 行间距更大
	cellPad := math.Max(30, float64(maxW)*0.1)
	cellH := float64(maxH) + gapY

	// 为每层的节点分配 x 坐标（层内居中对齐）
	for lv := 0; lv <= maxLayer; lv++ {
		ids := byLayer[lv]
		if len(ids) == 0 {
			continue
		}

		// 计算层的总宽度
		totalWidth := 0
		for _, id := range ids {
			w, _ := GetCardDimensions(nodeByID[id].Type)
			totalWidth += w
		}
		totalWidth += int(gapX) * (len(ids) - 1)

		// 居中偏移
		offsetX := cellPad

		x := offsetX
		for _, id := range ids {
			n := nodeByID[id]
			w, h := GetCardDimensions(n.Type)
			y := cellPad + float64(lv)*cellH + (cellH-float64(h))/2

			if n.Cfg == nil {
				n.Cfg = make(map[string]interface{})
			}
			n.Cfg["pos"] = map[string]interface{}{
				"x":      int(x),
				"y":      int(y),
				"width":  w,
				"height": h,
			}
			x += float64(w) + gapX
		}
	}
}

// barycenter 排序：根据参考层的顺序调整当前层
func barycenterSort(layer, refLayer []string, forward, backward map[string][]string, nodeByID map[string]*SceneNode) {
	// 计算参考层中每个节点的位置
	refPos := make(map[string]int)
	for i, id := range refLayer {
		refPos[id] = i
	}

	// 计算每个节点的 barycenter
	type nodeBC struct {
		id string
		bc float64
	}
	var items []nodeBC

	for _, id := range layer {
		preds := backward[id]
		if len(preds) == 0 {
			// 无前驱，用默认列
			items = append(items, nodeBC{id, float64(typeCol[nodeByID[id].Type])})
			continue
		}

		sum := 0.0
		count := 0
		for _, p := range preds {
			if pos, ok := refPos[p]; ok {
				sum += float64(pos)
				count++
			}
		}
		if count > 0 {
			items = append(items, nodeBC{id, sum / float64(count)})
		} else {
			items = append(items, nodeBC{id, float64(typeCol[nodeByID[id].Type])})
		}
	}

	// 按 barycenter 排序
	sort.Slice(items, func(i, j int) bool {
		if items[i].bc == items[j].bc {
			// 相同时按 typeCol 排序
			return typeCol[nodeByID[items[i].id].Type] < typeCol[nodeByID[items[j].id].Type]
		}
		return items[i].bc < items[j].bc
	})

	// 更新层顺序
	for i, item := range items {
		layer[i] = item.id
	}
}

// countCrossings 计算交叉数
func countCrossings(byLayer map[int][]string, forward map[string][]string, nodeByID map[string]*SceneNode) int {
	crossings := 0
	nodePos := make(map[string]map[string]int) // layer -> {id: position}

	for lv, ids := range byLayer {
		nodePos[fmt.Sprintf("%d", lv)] = make(map[string]int)
		for i, id := range ids {
			nodePos[fmt.Sprintf("%d", lv)][id] = i
		}
	}

	// 对每对相邻层，计算交叉
	for lv := 0; lv < len(byLayer)-1; lv++ {
		upper := byLayer[lv]
		for i := 0; i < len(upper); i++ {
			for j := i + 1; j < len(upper); j++ {
				// 检查 upper[i] 和 upper[j] 的边是否交叉
				for _, ti := range forward[upper[i]] {
					if nodePos[fmt.Sprintf("%d", lv+1)] == nil || nodePos[fmt.Sprintf("%d", lv+1)][ti] == 0 && ti != byLayer[lv+1][0] {
						continue
					}
					for _, tj := range forward[upper[j]] {
						if nodePos[fmt.Sprintf("%d", lv+1)] == nil {
							continue
						}
						pi, piok := nodePos[fmt.Sprintf("%d", lv+1)][ti]
						pj, pjok := nodePos[fmt.Sprintf("%d", lv+1)][tj]
						if !piok || !pjok {
							continue
						}
						if pi > pj {
							crossings++
						}
					}
				}
			}
		}
	}
	return crossings
}

// === 拓扑和分层 ===

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
				if idx := indexOf(t, "."); idx >= 0 {
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
				if idx := indexOf(t, "."); idx >= 0 {
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

func indexOf(s, sep string) int {
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}
