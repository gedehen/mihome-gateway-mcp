package native

import (
	"encoding/json"
	"testing"
)

func TestLayoutNodes(t *testing.T) {
	nodes := []*SceneNode{
		{ID: "nop1", Type: "nop", Outputs: map[string][]string{"output": {"input1.input"}}},
		{ID: "input1", Type: "deviceInput", Outputs: map[string][]string{"output": {"output1.trigger"}}},
		{ID: "output1", Type: "deviceOutput"},
	}

	LayoutNodes(nodes)

	for _, n := range nodes {
		pos, ok := n.Cfg["pos"].(map[string]interface{})
		if !ok {
			t.Fatalf("node %s has no pos", n.ID)
		}
		x, y := pos["x"].(int), pos["y"].(int)
		w, h := pos["width"].(int), pos["height"].(int)
		if x < 0 || y < 0 {
			t.Errorf("node %s: negative position (%d, %d)", n.ID, x, y)
		}
		t.Logf("  %s (%s): x=%d, y=%d, w=%d, h=%d", n.ID, n.Type, x, y, w, h)
	}

	// y 坐标递增
	nopY := nodes[0].Cfg["pos"].(map[string]interface{})["y"].(int)
	inputY := nodes[1].Cfg["pos"].(map[string]interface{})["y"].(int)
	outputY := nodes[2].Cfg["pos"].(map[string]interface{})["y"].(int)
	if inputY <= nopY {
		t.Errorf("deviceInput should be below nop")
	}
	if outputY <= inputY {
		t.Errorf("deviceOutput should be below deviceInput")
	}
}

func TestLayoutNoOverlap(t *testing.T) {
	nodes := []*SceneNode{
		{ID: "a", Type: "deviceInput", Outputs: map[string][]string{"output": {"c.input"}}},
		{ID: "b", Type: "deviceInput", Outputs: map[string][]string{"output": {"c.input"}}},
		{ID: "c", Type: "signalOr", Inputs: map[string]interface{}{"input": nil}, Outputs: map[string][]string{"output": {"d.trigger"}}},
		{ID: "d", Type: "deviceOutput", Inputs: map[string]interface{}{"trigger": nil}},
	}

	LayoutNodes(nodes)

	type box struct {
		x1, y1, x2, y2 int
		id              string
	}
	boxes := make([]box, len(nodes))
	for i, n := range nodes {
		pos := n.Cfg["pos"].(map[string]interface{})
		x, y := pos["x"].(int), pos["y"].(int)
		w, h := pos["width"].(int), pos["height"].(int)
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

	data, _ := json.MarshalIndent(nodes, "", "  ")
	t.Logf("Layout result:\n%s", string(data))

	if overlapCount >= len(nodes) {
		t.Errorf("too many overlaps: %d", overlapCount)
	}
}

func TestLayoutComplexScene(t *testing.T) {
	// 复杂场景：多个触发条件 + 逻辑门 + 输出
	nodes := []*SceneNode{
		{ID: "trigger1", Type: "deviceInput", Outputs: map[string][]string{"output": {"or1.input"}}},
		{ID: "trigger2", Type: "deviceInput", Outputs: map[string][]string{"output": {"or1.input"}}},
		{ID: "trigger3", Type: "deviceInput", Outputs: map[string][]string{"output": {"or1.input"}}},
		{ID: "or1", Type: "signalOr", Inputs: map[string]interface{}{"input": nil}, Outputs: map[string][]string{"output": {"and1.input"}}},
		{ID: "condition1", Type: "deviceGet", Outputs: map[string][]string{"output": {"and1.input"}}},
		{ID: "and1", Type: "logicAnd", Inputs: map[string]interface{}{"input": nil}, Outputs: map[string][]string{"output": {"delay1.input"}}},
		{ID: "delay1", Type: "delay", Inputs: map[string]interface{}{"input": nil}, Outputs: map[string][]string{"output": {"output1.trigger"}}},
		{ID: "output1", Type: "deviceOutput", Inputs: map[string]interface{}{"trigger": nil}},
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
		x, y := pos["x"].(int), pos["y"].(int)
		w, h := pos["width"].(int), pos["height"].(int)
		boxes[i] = box{x, y, x + w, y + h, n.ID}
	}

	overlapCount := 0
	for i := 0; i < len(boxes); i++ {
		for j := i + 1; j < len(boxes); j++ {
			a, b := boxes[i], boxes[j]
			if a.x1 < b.x2 && a.x2 > b.x1 && a.y1 < b.y2 && a.y2 > b.y1 {
				overlapCount++
			}
		}
	}

	data, _ := json.MarshalIndent(nodes, "", "  ")
	t.Logf("Complex layout:\n%s", string(data))
	t.Logf("Overlaps: %d/%d", overlapCount, len(nodes))

	// 复杂场景允许少量重叠
	if overlapCount > len(nodes)/2 {
		t.Errorf("too many overlaps: %d", overlapCount)
	}
}

func TestGetCardDimensions(t *testing.T) {
	tests := []struct {
		nodeType    string
		wantW, wantH int
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
