package server

import (
	"testing"

	"github.com/Autumn-27/artex/internal/edge"
)

// stallAdvance 的归零语义是熔断③的命门：0.13.1 及之前，fireEdgeWake 正常路径
// 从不把 StallLastHash 落库，loadEdgeBreaker 每次读回空 ⇒ 本函数恒走「基线」
// 分支返回 0 —— 停滞熔断是死代码（0.13.2 修复：推进移入水位 gate 且推进即
// save；本测试钉死计数语义，防止回归）。
func TestStallAdvance(t *testing.T) {
	const h1 = "aaaa"
	const h2 = "bbbb"

	// 基线：首次见到（lastHash 为空）算推进，返回 0。
	if got := stallAdvance("", h1, 0); got != 0 {
		t.Fatalf("基线轮应返回 0，got %d", got)
	}
	// 哈希为空（理论不可达：CriterionHash 对空 map 也产出值）时保守归零。
	if got := stallAdvance(h1, "", 3); got != 0 {
		t.Fatalf("空哈希应归零，got %d", got)
	}
	// 连续无推进：逐轮 +1，到达阈值即熔断。
	rounds := 0
	for i := 0; i < edge.StallThreshold; i++ {
		rounds = stallAdvance(h1, h1, rounds)
	}
	if rounds != edge.StallThreshold {
		t.Fatalf("连续 %d 轮无推进应到达阈值 %d，got %d", edge.StallThreshold, edge.StallThreshold, rounds)
	}
	// 任一推进（哈希变化）即清零。
	if got := stallAdvance(h1, h2, edge.StallThreshold); got != 0 {
		t.Fatalf("哈希变化应清零，got %d", got)
	}
}
