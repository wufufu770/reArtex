package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEgressGovernanceStateIsNeverGovernedWhileUncovered pins the核心不变式：
// 未覆盖通道清单非空 ⇒ governed 必为 false。这条一旦回归，UI 就会在列着未覆盖
// 通道的同时说出「出网已管控」—— 那正是「虚假安全感」。
func TestEgressGovernanceStateIsNeverGovernedWhileUncovered(t *testing.T) {
	state := CurrentEgressGovernanceState()
	if len(state.UncoveredChannels) == 0 {
		t.Fatal("当前出网治理必然存在未覆盖通道，清单不能为空")
	}
	if state.Governed {
		t.Fatal("未覆盖通道清单非空时 governed 必须是 false")
	}
	// 序列化之后不变式依然成立（UI 读的就是这份 JSON）。
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Governed          bool     `json:"governed"`
		UncoveredChannels []string `json:"uncovered_channels"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Governed {
		t.Fatalf("序列化后仍不得声称已治理: %s", b)
	}
	if len(payload.UncoveredChannels) != len(state.UncoveredChannels) {
		t.Fatalf("未覆盖通道清单在序列化后丢了: %s", b)
	}
}

// TestEgressGovernanceStateListsRequiredChannels pins 清单必须点名的那几条：
// 少写一条就等于把一个已知的缺口藏起来。
func TestEgressGovernanceStateListsRequiredChannels(t *testing.T) {
	state := CurrentEgressGovernanceState()
	joined := strings.Join(state.UncoveredChannels, "\n")
	required := []struct {
		name string
		want string
	}{
		{"web_fetch / web_search 直连", "web_fetch / web_search"},
		{"web_fetch 不吃 http_proxy", "http_proxy"},
		{"绕过 netguard transport", "netguard transport"},
		{"MCP 的外部通道", "MCP 的外部通道"},
		{"宿主侧出网", "宿主侧出网"},
		{"内置 deny 规则只挡破坏性命令", "只挡破坏性命令"},
		{"内置 deny 规则不挡范围外出网", "不挡范围外出网"},
		{"数据外泄管道规则出厂即 disabled", "数据外泄管道"},
		{"数据外泄管道规则未在生效", "未在生效"},
		{"disabled 出厂状态", "disabled"},
	}
	for _, r := range required {
		if !strings.Contains(joined, r.want) {
			t.Fatalf("未覆盖通道清单缺 %s（期望含 %q）:\n%s", r.name, r.want, joined)
		}
	}
}

// TestEgressGovernanceStateCannotClaimGoverned pins that no code path can
// produce a "fully governed" claim while the list is non-empty —— 无论是手工
// 构造的 struct、手写的 JSON，还是反序列化。
func TestEgressGovernanceStateCannotClaimGoverned(t *testing.T) {
	// 手工构造：字段是导出的，谁都能写 true。
	forged := EgressGovernanceState{Governed: true, UncoveredChannels: []string{"还有一条没盖住"}}
	b, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"governed":false`) {
		t.Fatalf("序列化必须由清单派生 governed，实际：%s", b)
	}
	// 手写 JSON：反序列化也必须由清单派生。
	var back EgressGovernanceState
	if err := json.Unmarshal([]byte(`{"governed":true,"uncovered_channels":["还有一条没盖住"]}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Governed {
		t.Fatal("反序列化不得接受「清单非空却已治理」")
	}
	// 清单为空时 governed 才能是 true（将来真的全覆盖了才会出现）。
	empty := newEgressGovernanceState(nil)
	if !empty.Governed {
		t.Fatal("清单为空时 governed 应为 true")
	}
	if len(empty.UncoveredChannels) != 0 {
		t.Fatal("空清单就是空清单")
	}
}

// TestEgressGovernanceRouteReturnsHonestState pins 路由回的就是这份契约。
func TestEgressGovernanceRouteReturnsHonestState(t *testing.T) {
	mux := http.NewServeMux()
	RegisterEgressGovernanceRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/settings/egress-governance", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var payload struct {
		Governed          bool     `json:"governed"`
		UncoveredChannels []string `json:"uncovered_channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Governed {
		t.Fatal("接口不得回「已治理」")
	}
	if len(payload.UncoveredChannels) == 0 {
		t.Fatal("接口必须回未覆盖通道清单")
	}
}
