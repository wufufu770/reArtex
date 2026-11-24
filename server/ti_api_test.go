package server

// TI-11 task-scoped API 第一阶段的验收用例（§0.11）：
// 路由可达性（真实路由表）、读端点形状、写端点的服务器端校验与指纹去重、
// 不存在任务的 404。派发器/Provider 接线不在本阶段（如实空态）。

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// tiAPIServer 复用 sodAPIServer 的测试装置（同库、同鉴权、真实路由表）。
func tiAPIServer(t *testing.T) (*db.DB, http.Handler, string) {
	t.Helper()
	pg, _, h, tok := sodAPIServer(t)
	return pg, h, tok
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// 主成功路径：六端点可达 + 快照端点如实声明 providers_wired=false + 写读一致。
func TestTaskIntelligenceAPIRoundTrip(t *testing.T) {
	pg, h, tok := tiAPIServer(t)
	tk, err := pg.CreateTask("TI-11 API 往返", "目标", nil, 0, 0)
	if err != nil {
		t.Skipf("无法建任务（%v）— 跳过", err)
	}
	defer pg.DeleteTask(tk.ID)
	path := func(suffix string) string { return "/api/tasks/" + itoa64(tk.ID) + suffix }

	// intelligence：契约形状 + 未接线如实声明。
	code, body := sodAPIDo(t, h, tok, "GET", path("/intelligence"), nil)
	if code != 200 {
		t.Fatalf("GET intelligence: %d %v", code, body)
	}
	if body["contract_version"] != "ti.context.v1" {
		t.Fatalf("契约版本不符: %v", body["contract_version"])
	}
	if body["providers_wired"] != false {
		t.Fatalf("Provider 未接线应如实声明 false: %v", body["providers_wired"])
	}

	// POST hypotheses：服务器端算指纹 + 水位。
	code, body = sodAPIDo(t, h, tok, "POST", path("/hypotheses"), map[string]any{
		"statement": "普通用户可能执行越权状态转换", "source": "TI-11 测试",
	})
	if code != 200 {
		t.Fatalf("POST hypotheses: %d %v", code, body)
	}
	id1, _ := body["id"].(float64)
	if id1 <= 0 {
		t.Fatalf("应返回正 id: %v", body)
	}

	// 指纹去重：同语句重投 → deduped=true 且 id 不变。
	code, body = sodAPIDo(t, h, tok, "POST", path("/hypotheses"), map[string]any{
		"statement": "普通用户可能执行越权状态转换", "source": "第二次提案",
	})
	if code != 200 || body["deduped"] != true {
		t.Fatalf("同语句应去重: %d %v", code, body)
	}
	if id2, _ := body["id"].(float64); id2 != id1 {
		t.Fatalf("去重应返回既有 id: %v vs %v", id1, id2)
	}

	// GET hypotheses 读回。
	code, body = sodAPIDo(t, h, tok, "GET", path("/hypotheses"), nil)
	if code != 200 {
		t.Fatalf("GET hypotheses: %d %v", code, body)
	}

	// model-deltas 复用 biz_layer_diffs（空列表也算成功形状）。
	code, body = sodAPIDo(t, h, tok, "GET", path("/model-deltas"), nil)
	if code != 200 {
		t.Fatalf("GET model-deltas: %d %v", code, body)
	}

	// capability-runs / timeline。
	if code, _ := sodAPIDo(t, h, tok, "GET", path("/capability-runs"), nil); code != 200 {
		t.Fatalf("GET capability-runs: %d", code)
	}
	code, body = sodAPIDo(t, h, tok, "GET", path("/timeline"), nil)
	if code != 200 {
		t.Fatalf("GET timeline: %d %v", code, body)
	}
	if tl, ok := body["timeline"].([]any); !ok || len(tl) < 1 {
		t.Fatalf("时间线应含刚写入的假设: %v", body)
	}
}

// 失败路径：statement 缺失 400（对**存在的任务**）；不存在的任务 404——
// requireTask 的重载校验先于参数校验，顺序本身就是 §0.11 的契约。
func TestTaskIntelligenceAPIGuards(t *testing.T) {
	pg, h, tok := tiAPIServer(t)
	tk, err := pg.CreateTask("TI-11 守卫", "目标", nil, 0, 0)
	if err != nil {
		t.Skipf("无法建任务（%v）— 跳过", err)
	}
	defer pg.DeleteTask(tk.ID)

	// 缺 statement（对存在任务：参数校验生效）。
	code, body := sodAPIDo(t, h, tok, "POST", "/api/tasks/"+itoa64(tk.ID)+"/hypotheses", map[string]any{})
	if code != 400 {
		t.Fatalf("缺 statement 应 400: %d %v", code, body)
	}
	// 不存在的任务（读+写都走同一 requireTask 门）。
	for _, spec := range [][2]string{
		{"GET", "/api/tasks/999999/hypotheses"},
		{"POST", "/api/tasks/999999/hypotheses"},
		{"GET", "/api/tasks/999999/intelligence"},
		{"GET", "/api/tasks/999999/timeline"},
	} {
		if code, _ := sodAPIDo(t, h, tok, spec[0], spec[1], nil); code != 404 {
			t.Fatalf("%s %s 应 404, got %d", spec[0], spec[1], code)
		}
	}
	// 非法任务 id。
	if code, _ := sodAPIDo(t, h, tok, "GET", "/api/tasks/not-a-number/hypotheses", nil); code != 400 {
		t.Fatalf("非法 id 应 400, got %d", code)
	}
}
