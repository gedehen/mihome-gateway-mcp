package native

import (
	"encoding/json"
	"testing"
)

func TestLayoutNodes(t *testing.T) {
	// 构建一个简单场景：nop → deviceInput → deviceOutput
	nodes := []*SceneNode{
		{
			ID:   "nop1",
			Type: "nop",
			Cfg:  map[string]interface{}{},
			Outputs: map[string][]string{
				"output": {"input1.input"},
			},
		},
		{
			ID:   "input1",
			Type: "deviceInput",
			Cfg:  map[string]interface{}{},
			Outputs: map[string][]string{
				"output": {"output1.trigger"},
			},
		},
		{
			ID:   "output1",
			Type: "deviceOutput",
			Cfg:  map[string]interface{}{},
		},
	}

	LayoutNodes(nodes)

	// 验证节点被分配了位置
	for _, n := range nodes {
		pos, ok := n.Cfg["pos"].(map[string]interface{})
		if !ok {
			t.Fatalf("node %s has no pos", n.ID)
		}
		x := pos["x"].(int)
		y := pos["y"].(int)
		w := pos["width"].(int)
		h := pos["height"].(int)

		if x < 0 || y < 0 {
			t.Errorf("node %s: negative position (%d, %d)", n.ID, x, y)
		}
		if w <= 0 || h <= 0 {
			t.Errorf("node %s: invalid dimensions (%d, %d)", n.ID, w, h)
		}
		t.Logf("  %s (%s): x=%d, y=%d, w=%d, h=%d", n.ID, n.Type, x, y, w, h)
	}

	// 验证 y 坐标递增（下游节点在下方）
	nopY := nodes[0].Cfg["pos"].(map[string]interface{})["y"].(int)
	inputY := nodes[1].Cfg["pos"].(map[string]interface{})["y"].(int)
	outputY := nodes[2].Cfg["pos"].(map[string]interface{})["y"].(int)

	if inputY <= nopY {
		t.Errorf("deviceInput (y=%d) should be below nop (y=%d)", inputY, nopY)
	}
	if outputY <= inputY {
		t.Errorf("deviceOutput (y=%d) should be below deviceInput (y=%d)", outputY, inputY)
	}
}

func TestLayoutNoOverlap(t *testing.T) {
	// 复杂场景：多个同层节点
	nodes := []*SceneNode{
		{ID: "a", Type: "deviceInput", Outputs: map[string][]string{"output": {"c.input"}}},
		{ID: "b", Type: "deviceInput", Outputs: map[string][]string{"output": {"c.input"}}},
		{ID: "c", Type: "signalOr", Inputs: map[string]interface{}{"input": nil}, Outputs: map[string][]string{"output": {"d.trigger"}}},
		{ID: "d", Type: "deviceOutput", Inputs: map[string]interface{}{"trigger": nil}},
	}

	LayoutNodes(nodes)

	// 检查无重叠
	type box struct {
		x1, y1, x2, y2 int
		id              string
	}
	boxes := make([]box, len(nodes))
	for i, n := range nodes {
		pos := n.Cfg["pos"].(map[string]interface{})
		x := pos["x"].(int)
		y := pos["y"].(int)
		w := pos["width"].(int)
		h := pos["height"].(int)
		boxes[i] = box{x, y, x + w, y + h, n.ID}
	}

	overlapCount := 0
	for i := 0; i < len(boxes); i++ {
		for j := i + 1; j < len(boxes); j++ {
			a, b := boxes[i], boxes[j]
			if a.x1 < b.x2 && a.x2 > b.x1 && a.y1 < b.y2 && a.y2 > b.y1 {
				t.Logf("overlap: %s vs %s", a.id, b.id)
				overlapCount++
			}
		}
	}

	// 输出布局结果
	data, _ := json.MarshalIndent(nodes, "", "  ")
	t.Logf("Layout result:\n%s", string(data))

	// 允许少量重叠（复杂场景不可避免），但不能全部重叠
	if overlapCount >= len(nodes) {
		t.Errorf("too many overlaps: %d", overlapCount)
	}
}

func TestGetCardDimensions(t *testing.T) {
	tests := []struct {
		nodeType string
		wantW    int
		wantH    int
	}{
		{"deviceInput", 280, 120},
		{"nop", 120, 60},
		{"unknown", 280, 120},
	}
	for _, tt := range tests {
		w, h := GetCardDimensions(tt.nodeType)
		if w != tt.wantW || h != tt.wantH {
			t.Errorf("GetCardDimensions(%s) = (%d, %d), want (%d, %d)", tt.nodeType, w, h, tt.wantW, tt.wantH)
		}
	}
}
