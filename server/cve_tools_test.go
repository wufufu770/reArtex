package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// ---------- 工具面 ----------

// 四工具、精确名单、只读标记；同步触发工具绝不在面内（cve_sync_status 是只读
// 状态视图，B-03E 契约；cve_sync_once / 手动 sync 一律不暴露给 Agent）。
func TestCveToolSurface(t *testing.T) {
	tools := new(Server).cveTools()
	if len(tools) != 4 {
		t.Fatalf("cveTools 必须 4 个，得到 %d", len(tools))
	}
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Name()] = true
		if tl.InputSchema() == nil {
			t.Fatalf("%s 缺 schema", tl.Name())
		}
	}
	for _, want := range cveToolNames {
		if !got[want] {
			t.Fatalf("缺工具 %s（实际 %v）", want, got)
		}
	}
	for _, banned := range []string{"cve_sync_once", "cve_sync_now", "cve_sync", "cve_sync_trigger", "sync_cve"} {
		if got[banned] {
			t.Fatalf("同步触发工具 %s 不得暴露给 Agent", banned)
		}
	}
	if got["cve_sync"] {
		t.Fatalf("裸名 cve_sync 不得出现（状态视图叫 cve_sync_status）")
	}
}

// ---------- B-01E 五情形：未配置 / 连接拒绝 / 超时 / 空结果 / 正常命中 ----------

// fakeCveMCP 是最小 Streamable MCP fake（cveintel/client_test.go 同款信封纪律）。
type fakeCveMCP struct {
	srv        *httptest.Server
	status     int
	body       string // 覆盖全部工具的固定响应体
	searchBody string // 仅 cve_search 的响应体（其余工具走默认分支）
	delay      time.Duration
}

func newFakeCveMCP(t *testing.T) *fakeCveMCP {
	t.Helper()
	f := &fakeCveMCP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if f.status != 0 && f.status != http.StatusOK {
			http.Error(w, "boom", f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 请求体只解码一次（body 流不可重复读——searchBody 分支落在默认分支时
		// 二次 Decode 会得到空 name，误入 "{}" 兜底）。
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.searchBody != "" && req.Params.Name == "cve_search" {
			_, _ = w.Write([]byte(f.searchBody))
			return
		}
		if f.body != "" {
			_, _ = w.Write([]byte(f.body))
			return
		}
		var text string
		switch req.Params.Name {
		case "cve_lookup":
			text = `{"schema_version":"cve_intel.v1","cve_id":"CVE-2021-44228","title":"Log4j","cwe_ids":[],"in_cisa_kev":true,"affected_ranges":[],"fixed_versions":[],"references":[],"related_skills":[],"source_provenance":"nvd"}`
		case "cve_search":
			text = `[{"cve_id":"CVE-2024-0001"}]`
		case "cve_match_component":
			text = `[{"verdict":"affected","reason":"matched range"}]`
		case "cve_sync_status":
			text = `{"sources":{"kev":{"last_sync":"","last_success":"","last_attempt":"","last_error":"","records":0,"stale":true,"status":"unknown"}},"total_records":0}`
		default:
			text = `{}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// cveToolCall 走真实工具 Run（roTool 包装层以内即 Run 本体），返回内层 JSON 文本
// （工具结果的 text 块内容，断言针对它做）。
func cveToolCall(t *testing.T, srv *Server, name string, in string) string {
	t.Helper()
	for _, tl := range srv.cveTools() {
		if tl.Name() != name {
			continue
		}
		res, err := tl.Call(context.Background(), json.RawMessage(in), nil)
		if err != nil {
			t.Fatalf("%s Call 返回 error（工具异常必须落结果不落 error）: %v", name, err)
		}
		var wire struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"Content"`
		}
		b, _ := json.Marshal(res)
		if json.Unmarshal(b, &wire) != nil || len(wire.Content) == 0 {
			t.Fatalf("%s 结果缺 text 块: %s", name, b)
		}
		return wire.Content[0].Text
	}
	t.Fatalf("找不到工具 %s", name)
	return ""
}

func assertUnavailable(t *testing.T, name, out string) {
	t.Helper()
	if !strings.Contains(out, `"status":"unavailable"`) {
		t.Fatalf("%s 未显式 unavailable: %s", name, out)
	}
	if strings.Contains(out, `"records":[`) || strings.Contains(out, `"found":true`) {
		t.Fatalf("%s 把 unavailable 伪装成了结果: %s", name, out)
	}
}

// 五情形 + 不把 unavailable 伪装成结果（B-01E 验收逐项）。
func TestCveToolsUnavailableSemantics(t *testing.T) {
	srv := new(Server)
	lookupIn := `{"cve_id":"CVE-2021-44228"}`

	// ① 未配置：显式 unavailable，绝不动伪造结果兜底。
	t.Setenv("CVE_MCP_URL", "")
	assertUnavailable(t, "lookup/未配置", cveToolCall(t, srv, "cve_lookup", lookupIn))
	assertUnavailable(t, "search/未配置", cveToolCall(t, srv, "cve_search", `{"query":"log4j"}`))
	assertUnavailable(t, "match/未配置", cveToolCall(t, srv, "cve_match_component", `{"package_or_cpe":"log4j-core","version":"2.14.1"}`))
	assertUnavailable(t, "status/未配置", cveToolCall(t, srv, "cve_sync_status", `{}`))

	// ② 连接拒绝：显式 unavailable。
	t.Setenv("CVE_MCP_URL", "http://127.0.0.1:1")
	assertUnavailable(t, "lookup/拒绝", cveToolCall(t, srv, "cve_lookup", lookupIn))

	// ③ 上游 5xx：显式 unavailable。
	f5xx := newFakeCveMCP(t)
	f5xx.status = http.StatusInternalServerError
	t.Setenv("CVE_MCP_URL", f5xx.srv.URL)
	assertUnavailable(t, "lookup/500", cveToolCall(t, srv, "cve_lookup", lookupIn))

	// ④ 超时：注入短超时客户端 + 慢 fake —— 显式 unavailable（生产路径超时归
	// cveintel 缺省 5s；这里经 newCveIntelClient 钩子换 50ms 以便快速实证同一
	// ErrUnavailable→unavailable 映射分支）。
	fSlow := newFakeCveMCP(t)
	fSlow.delay = 300 * time.Millisecond
	t.Setenv("CVE_MCP_URL", fSlow.srv.URL)
	origNew := newCveIntelClient
	newCveIntelClient = func(base string) (*cveintel.Client, error) {
		c, err := cveintel.New(base)
		if err != nil {
			return nil, err
		}
		return c.WithTimeout(50 * time.Millisecond), nil
	}
	assertUnavailable(t, "lookup/超时", cveToolCall(t, srv, "cve_lookup", lookupIn))
	newCveIntelClient = origNew

	// ⑤a 空查询结果：显式 status:ok + 空数组 —— 空结果与 unavailable 绝不混同。
	fEmpty := newFakeCveMCP(t)
	fEmpty.body = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[]"}]}}`
	t.Setenv("CVE_MCP_URL", fEmpty.srv.URL)
	out := cveToolCall(t, srv, "cve_search", `{"query":"nothing-matches"}`)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, `"records":[]`) {
		t.Fatalf("空结果必须显式 ok+空数组: %s", out)
	}

	// ⑤b 正常命中：结构化情报 + 三态判定原样透传。
	fHit := newFakeCveMCP(t)
	t.Setenv("CVE_MCP_URL", fHit.srv.URL)
	out = cveToolCall(t, srv, "cve_lookup", lookupIn)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, `"in_cisa_kev":true`) {
		t.Fatalf("lookup 命中必须 ok+KEV 标记: %s", out)
	}
	out = cveToolCall(t, srv, "cve_match_component", `{"package_or_cpe":"log4j-core","ecosystem":"maven","version":"2.14.1"}`)
	if !strings.Contains(out, `"verdict":"affected"`) {
		t.Fatalf("match 三态必须原样透传: %s", out)
	}
	out = cveToolCall(t, srv, "cve_sync_status", `{}`)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, `"sources"`) {
		t.Fatalf("status 必须回三源明细: %s", out)
	}

	// 参数错误族：fail-closed 显式报错，绝不静默纠偏（B-03C 同纪律）。
	if out := cveToolCall(t, srv, "cve_search", `{"query":"x","limit":0}`); strings.Contains(out, `"records"`) {
		t.Fatalf("limit=0 必须报错不走默认: %s", out)
	}
	if out := cveToolCall(t, srv, "cve_search", `{"query":"x","limit":101}`); !strings.Contains(out, "limit") {
		t.Fatalf("limit>100 必须显式报错: %s", out)
	}
	if out := cveToolCall(t, srv, "cve_lookup", `{"cve_id":"  "}`); !strings.Contains(out, "必填") {
		t.Fatalf("空白 cve_id 必须报必填: %s", out)
	}
	if out := cveToolCall(t, srv, "cve_match_component", `{"package_or_cpe":"p","ranges":"not-array"}`); !strings.Contains(out, "非法") {
		t.Fatalf("ranges 非法形态必须显式报错: %s", out)
	}
}

// ---------- 绑定 / 延迟披露 / 真实装配链（live PG，不可用即跳过） ----------

func cveTestPG(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// 种子行为：行存在、启用、默认零绑定（最小 scope，零行为变化）。
func TestCveToolsSeedDefaults(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{}
	srv.m = &Manager{pg: pg}
	srv.seedCveTools()
	for _, name := range cveToolNames {
		row, err := pg.GetTool(name)
		if err != nil || row == nil {
			t.Fatalf("%s 未播种: %v", name, err)
		}
		if !row.Enabled {
			t.Fatalf("%s 种子行必须默认启用", name)
		}
		if len(row.Agents) != 0 {
			t.Fatalf("%s 种子行必须默认零绑定（得到 %v）", name, row.Agents)
		}
	}
	// 未绑定 ⇒ 不进任何披露面。
	if binds := srv.cveDeferredBinds(); len(binds) != 4 {
		t.Fatalf("cveDeferredBinds 应有 4 键（未绑定时空数组），得到 %v", binds)
	} else {
		for _, name := range cveToolNames {
			if len(binds[name]) != 0 {
				t.Fatalf("%s 未绑定却出现在披露绑定: %v", name, binds[name])
			}
		}
	}
}

// 绑定 ↔ 披露 ↔ 调用面：绑定后 ToolResolve 保留、未绑/停用即剥；deferred 披露
// 跟随绑定。走真实 wireTools 过滤路径（tools_wire_test 同款）。
func TestCveToolsBindingAndDisclosure(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{}
	srv.m = &Manager{pg: pg}
	srv.seedCveTools()
	wireTools(pg, nil)
	t.Cleanup(func() { agent.ToolResolve = nil })

	// 绑定 worker → cveDeferredBinds 披露 + ToolResolve 保留。
	if err := pg.AddAgentToToolBinding("worker", cveToolNames); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range cveToolNames {
			_ = pg.RemoveAgentFromTool("worker", name)
		}
		agent.InvalidateAssembly()
	})
	agent.InvalidateAssembly()

	binds := srv.cveDeferredBinds()
	for _, name := range cveToolNames {
		if len(binds[name]) != 1 || binds[name][0] != "worker" {
			t.Fatalf("绑定后 %s 披露绑定错误: %v", name, binds[name])
		}
	}

	cve := names(srv.cveTools())
	base := make([]actool.CoreTool, 0, len(cve)+1)
	for _, tl := range srv.cveTools() {
		base = append(base, tl)
	}
	base = append(base, actool.NewBash()) // 对照组：SDK 工具原样透传
	ctx := context.Background()
	worker := names(agent.ToolResolve(ctx, "worker", base))
	for _, name := range cveToolNames {
		if _, ok := worker[name]; !ok {
			t.Fatalf("绑定的 worker 丢失 %s", name)
		}
	}
	if _, ok := worker["Bash"]; !ok {
		t.Fatal("对照组 Bash 应原样透传")
	}
	// 未绑定的 auto（非角色收敛外的校验无关角色，只看绑定过滤）拿不到。
	others := names(agent.ToolResolve(ctx, "auto", base))
	for _, name := range cveToolNames {
		if _, ok := others[name]; ok {
			t.Fatalf("未绑定的 auto 不应拿到 %s（未披露即不可调用）", name)
		}
	}
	// 行停用 → 即使绑定也剥（UI 关闸即时生效）。
	row, _ := pg.GetTool("cve_lookup")
	if err := pg.UpdateTool("cve_lookup", row.Description, row.Schema, mustJSON([]string{"worker"}), false); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	t.Cleanup(func() {
		_ = pg.UpsertToolForce("cve_lookup", row.Description, row.Schema, mustJSON([]string{"worker"}))
	})
	worker = names(agent.ToolResolve(ctx, "worker", base))
	if _, ok := worker["cve_lookup"]; ok {
		t.Fatal("停用行必须从调用面剥除（即使绑定）")
	}
	// 停用行也不进披露面。
	binds = srv.cveDeferredBinds()
	if _, disclosed := binds["cve_lookup"]; disclosed {
		t.Fatal("停用行不得进 deferred 披露面")
	}
}

// 真实装配链（验收「四工具可经安全链执行、未授权不可调用」）：wireAgentAugment +
// AugmentTools 全链 —— hostTools 提供器 → ToolAugment → ToolResolve →
// filterToolsForRole → enforceToolPolicy/guardPanic/SoD 统一装饰。
// provider 用最小面（仅 CVE 工具）聚焦本卡断言；真实启动路径是全量 hostTools。
func TestCveToolsFullAssemblyChain(t *testing.T) {
	pg := cveTestPG(t)
	srv := &Server{}
	srv.m = &Manager{pg: pg}
	srv.seedCveTools()
	if err := pg.AddAgentToToolBinding("worker", cveToolNames); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range cveToolNames {
			_ = pg.RemoveAgentFromTool("worker", name)
		}
		agent.ToolAugment = nil
		agent.InvalidateAssembly()
	})
	agent.InvalidateAssembly()

	wireAgentAugment(pg, "../skills", func() ([]actool.CoreTool, map[string][]string) {
		return srv.cveTools(), srv.cveDeferredBinds()
	})

	ctx := context.Background()

	// worker（绑定 + 不受角色白名单收敛）：四工具进装配面、全部过策略装饰、
	// 名字进 deferred 披露块。
	extra, def, cleanup := agent.AugmentTools(ctx, "worker", actool.DefaultTools())
	defer cleanup()
	for _, name := range cveToolNames {
		if _, ok := names(extra)[name]; !ok {
			t.Fatalf("装配面丢失 %s（worker 已绑定）", name)
		}
	}
	if !agent.IsPolicyWrapped(names(extra)["cve_lookup"]) {
		t.Fatal("cve_lookup 必须被 enforceToolPolicy 装饰（安全管道统一收口）")
	}
	defSet := map[string]bool{}
	for _, n := range def.Deferred {
		defSet[n] = true
	}
	for _, name := range cveToolNames {
		if !defSet[name] {
			t.Fatalf("%s 未进 deferred 披露名单 %v", name, def.Deferred)
		}
	}

	// planner（角色白名单收敛，cve_* 不在其允许集）：装配期主闸整组剥除——
	// 可调用面与披露面同时消失（未授权 ⇒ 不可见 ⇒ 不可调用）。
	pExtra, pDef, pCleanup := agent.AugmentTools(ctx, "planner", actool.DefaultTools())
	defer pCleanup()
	for _, tl := range pExtra {
		if strings.HasPrefix(tl.Name(), "cve_") {
			t.Fatalf("planner（角色收敛）不应看到 %s", tl.Name())
		}
	}
	for _, n := range pDef.Deferred {
		if strings.HasPrefix(n, "cve_") {
			t.Fatalf("planner 的披露面不应含 %s", n)
		}
	}
}
