package server

// 本文件是【API 侧写路径判定闸门】的验收测试。
//
// 验收口径（每个端点三条）：
//   (a) 工具面会接受的请求 ⇒ API 侧也接受；
//   (b) 工具面会拒绝的请求 ⇒ API 侧也拒绝，且**拒绝理由逐字相同**；
//   (c) 范围 / 策略查不到 ⇒ 拒绝（fail closed），绝不放行。
//
// 另有一条单独钉住【操作者侧 vs agent 侧】的区分：操作者声明不受 agent 扩权上限
// 约束、也不消耗 agent 的扩张预算；agent 侧通道受两者约束。
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// =====================================================================
// 测试脚手架
// =====================================================================

func guardTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// guardTestTask 建一个任务并登记清理。
func guardTestTask(t *testing.T, m *Manager, label string) int64 {
	t.Helper()
	task, err := m.CreateTaskWithOptions(label+" "+fmt.Sprint(time.Now().UnixNano()), "goal", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return mustTaskID(t, task.ID)
}

// guardScopeRows 读任务自己的范围行。
func guardScopeRows(t *testing.T, m *Manager, taskID int64) []db.TaskScope {
	t.Helper()
	rows, err := m.pg.Assets().ListTaskScope(taskID)
	if err != nil {
		t.Fatalf("list task scope: %v", err)
	}
	return rows
}

// =====================================================================
// 端点一：POST /api/tasks/{id}/scope（与工具面 add_task_scope 同副作用）
// =====================================================================

// TestGuardTaskScopeAddAgreesWithToolSurfaceOnEveryVerdict 是通道一致性的主验收：
// 同一批请求分别走工具面（直调 db.AddAgentScope —— agent/tools_insert.go 调的就是它）
// 与 API 闸门，两边的接受/拒绝必须一致，拒绝时理由必须逐字相同。
func TestGuardTaskScopeAddAgreesWithToolSurfaceOnEveryVerdict(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}

	cases := []struct {
		name string
		kind string
		// value 逐字传给两侧（工具面只 trim kind，不 trim value）。
		value string
		// policyDenied = 预期被 agent 侧扩权策略（kind 白名单）拒绝。
		policyDenied bool
	}{
		{name: "子域在白名单内", kind: "subdomain", value: "api.example.com"},
		{name: "IP 在白名单内", kind: "ip", value: "203.0.113.10"},
		{name: "关键词在白名单内", kind: "keyword", value: "Acme Security"},
		{name: "ICP 在白名单内", kind: "icp", value: "京ICP备123号"},
		{name: "根域必须人工确认", kind: "root_domain", value: "example.com", policyDenied: true},
		{name: "公司必须人工确认", kind: "company", value: "Acme", policyDenied: true},
		{name: "未知 kind", kind: "not_a_kind", value: "whatever"},
		{name: "空值", kind: "subdomain", value: "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 两个独立任务：两侧各写各的，扩权预算互不干扰。
			toolTask := guardTestTask(t, m, "tool surface")
			apiTask := guardTestTask(t, m, "api guard")

			_, toolErr := as.AddAgentScope(toolTask, strings.TrimSpace(tc.kind), tc.value, "理由", "agent")
			_, apiErr := s.guardTaskScopeAdd(as, apiTask, taskScopeAddRequest{Kind: tc.kind, Value: tc.value, Reason: "理由"})

			if (toolErr == nil) != (apiErr == nil) {
				t.Fatalf("接受/拒绝不一致：工具面 err=%v，API 闸门 err=%v", toolErr, apiErr)
			}
			if toolErr != nil {
				if toolErr.Error() != apiErr.Error() {
					t.Fatalf("拒绝理由不一致：\n工具面: %s\nAPI  : %s", toolErr.Error(), apiErr.Error())
				}
				if tc.policyDenied {
					var denied *db.ScopeExpansionDeniedError
					if !errors.As(apiErr, &denied) {
						t.Fatalf("必须是扩权策略拒绝（*db.ScopeExpansionDeniedError），实际: %v", apiErr)
					}
					if !strings.Contains(apiErr.Error(), "不得自行把范围扩到") {
						t.Fatalf("拒绝理由必须是中文策略理由，实际: %s", apiErr.Error())
					}
				}
				// 被拒 ⇒ 一行范围都不能落库。
				if rows := guardScopeRows(t, m, apiTask); len(rows) != 0 {
					t.Fatalf("被拒的请求落库了 %d 行范围", len(rows))
				}
				return
			}
			// 被接受 ⇒ 两侧都要真的落一行，且 kind 一致。
			toolRows := guardScopeRows(t, m, toolTask)
			apiRows := guardScopeRows(t, m, apiTask)
			if len(toolRows) != 1 || len(apiRows) != 1 {
				t.Fatalf("接受后应各落 1 行：工具面 %d 行，API %d 行", len(toolRows), len(apiRows))
			}
			if toolRows[0].Kind != apiRows[0].Kind {
				t.Fatalf("kind 不一致：工具面 %s，API %s", toolRows[0].Kind, apiRows[0].Kind)
			}
			// 两侧都是 agent 侧来源 ⇒ 数据上可分辨（操作者声明是 source='manual'）。
			if apiRows[0].Source != "agent" {
				t.Fatalf("API 闸门应写 agent 侧来源，实际 source=%q", apiRows[0].Source)
			}
		})
	}
}

// TestGuardTaskScopeAddEnforcesSameExpansionCapAsToolSurface 钉住扩权条数上限：
// 两侧都加满上限后，下一条必须以同一句中文理由被丢弃。
func TestGuardTaskScopeAddEnforcesSameExpansionCapAsToolSurface(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}

	toolTask := guardTestTask(t, m, "cap tool surface")
	apiTask := guardTestTask(t, m, "cap api guard")
	cap := db.AgentScopeExpansionCap()
	if cap <= 0 {
		t.Fatalf("扩权上限必须是正数，实际 %d", cap)
	}

	for i := 0; i < cap; i++ {
		domain := fmt.Sprintf("cap-%d-%d.example.com", time.Now().UnixNano(), i)
		if _, err := as.AddAgentScope(toolTask, "subdomain", domain, "理由", "agent"); err != nil {
			t.Fatalf("工具面第 %d 条被拒: %v", i+1, err)
		}
		if _, err := s.guardTaskScopeAdd(as, apiTask, taskScopeAddRequest{Kind: "subdomain", Value: domain, Reason: "理由"}); err != nil {
			t.Fatalf("API 闸门第 %d 条被拒: %v", i+1, err)
		}
	}

	overTool := fmt.Sprintf("cap-over-tool-%d.example.com", time.Now().UnixNano())
	overAPI := fmt.Sprintf("cap-over-api-%d.example.com", time.Now().UnixNano())
	_, toolErr := as.AddAgentScope(toolTask, "subdomain", overTool, "理由", "agent")
	_, apiErr := s.guardTaskScopeAdd(as, apiTask, taskScopeAddRequest{Kind: "subdomain", Value: overAPI, Reason: "理由"})
	if toolErr == nil || apiErr == nil {
		t.Fatalf("超限必须被拒：工具面 err=%v，API err=%v", toolErr, apiErr)
	}
	if toolErr.Error() != apiErr.Error() {
		t.Fatalf("超限拒绝理由不一致：\n工具面: %s\nAPI  : %s", toolErr.Error(), apiErr.Error())
	}
	if !strings.Contains(apiErr.Error(), "已达单任务扩权上限") {
		t.Fatalf("超限拒绝理由必须是中文上限理由，实际: %s", apiErr.Error())
	}
}

// TestGuardTaskScopeAddFailsClosedWhenScopeLookupFails 钉住 fail closed：
// 范围/预算查不到（数据库故障）⇒ 拒绝写入，绝不「出错就放行」。
func TestGuardTaskScopeAddFailsClosedWhenScopeLookupFails(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "fail closed scope")

	if err := m.pg.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := s.guardTaskScopeAdd(as, taskID, taskScopeAddRequest{Kind: "subdomain", Value: "fail-closed.example.com", Reason: "理由"}); err == nil {
		t.Fatal("数据库故障时必须拒绝写入（fail closed）")
	}
}

// =====================================================================
// 端点二：DELETE /api/tasks/{id}/scope/{sid}
// =====================================================================

// TestGuardTaskScopeDeleteOnlyDeletesOwnRows 钉住删除侧的范围归属判定：
// 别的任务的行删不掉（跨任务改授权边界），自己任务的行删得掉。
func TestGuardTaskScopeDeleteOnlyDeletesOwnRows(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}

	owner := guardTestTask(t, m, "scope owner")
	other := guardTestTask(t, m, "scope other")

	if _, err := s.guardTaskScopeAdd(as, owner, taskScopeAddRequest{Kind: "subdomain", Value: "owned.example.com", Reason: "理由"}); err != nil {
		t.Fatalf("owner scope add: %v", err)
	}
	if _, err := s.guardTaskScopeAdd(as, other, taskScopeAddRequest{Kind: "subdomain", Value: "foreign.example.com", Reason: "理由"}); err != nil {
		t.Fatalf("other scope add: %v", err)
	}
	ownerRows := guardScopeRows(t, m, owner)
	otherRows := guardScopeRows(t, m, other)
	if len(ownerRows) != 1 || len(otherRows) != 1 {
		t.Fatalf("前置条件不成立：owner %d 行，other %d 行", len(ownerRows), len(otherRows))
	}

	// 删别的任务的行 ⇒ 不删。
	deleted, err := s.guardTaskScopeDelete(as, owner, otherRows[0].ID)
	if err != nil {
		t.Fatalf("跨任务删除应返回「未删除」而不是错误，实际 err=%v", err)
	}
	if deleted {
		t.Fatal("跨任务的范围行不得被删除")
	}
	if rows := guardScopeRows(t, m, other); len(rows) != 1 {
		t.Fatalf("别的任务的范围行被删掉了，剩 %d 行", len(rows))
	}

	// 删自己的行 ⇒ 删掉。
	deleted, err = s.guardTaskScopeDelete(as, owner, ownerRows[0].ID)
	if err != nil {
		t.Fatalf("delete own scope: %v", err)
	}
	if !deleted {
		t.Fatal("本任务自己的范围行必须删得掉")
	}
	if rows := guardScopeRows(t, m, owner); len(rows) != 0 {
		t.Fatalf("范围行没被删掉，剩 %d 行", len(rows))
	}
}

// TestGuardTaskScopeDeleteFailsClosedWhenScopeLookupFails 钉住 fail closed：
// 归属查询失败 ⇒ 拒绝删除。
func TestGuardTaskScopeDeleteFailsClosedWhenScopeLookupFails(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "fail closed delete")

	if err := m.pg.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	deleted, err := s.guardTaskScopeDelete(as, taskID, 1)
	if err == nil {
		t.Fatal("数据库故障时必须拒绝删除（fail closed）")
	}
	if deleted {
		t.Fatal("数据库故障时不得报告删除成功")
	}
}

// =====================================================================
// 端点三：POST /api/tasks/{id}/assets（与工具面 insert_assets 同副作用）
// =====================================================================

// guardTestAsset 建一个全局资产（不进任何任务范围）。
func guardTestAsset(t *testing.T, m *Manager, domain string) int64 {
	t.Helper()
	id, err := m.pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: domain})
	if err != nil {
		t.Fatalf("upsert root domain %s: %v", domain, err)
	}
	return id
}

// TestGuardTaskAssetAttachRejectsBlockedAssetWithToolSurfaceReason 钉住资产闸门：
// 命中拦截规则的资产挂不上，且拒绝理由与工具面逐字相同。
func TestGuardTaskAssetAttachRejectsBlockedAssetWithToolSurfaceReason(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "gate")

	blocked := fmt.Sprintf("blocked-%d.example.com", time.Now().UnixNano())
	if _, err := m.pg.CreateAssetInterceptRule("exact_domain", blocked, "测试拦截", true); err != nil {
		t.Fatalf("create intercept rule: %v", err)
	}
	assetID := guardTestAsset(t, m, blocked)
	// 先把资产声明进任务范围（操作者侧声明），让判定只落在资产闸门上。
	if err := m.pg.AddTaskScopeAsset(taskID, assetID); err != nil {
		t.Fatalf("declare task scope asset: %v", err)
	}

	err := s.guardTaskAssetAttach(as, taskID, []int64{assetID})
	if err == nil {
		t.Fatal("命中拦截规则的资产不得挂载")
	}
	if !errors.Is(err, errTaskAssetGateDenied) {
		t.Fatalf("必须是资产闸门拒绝，实际: %v", err)
	}
	// 工具面同一条判定的理由必须一致。
	hits, toolErr := as.CheckAssetsIntercept(taskID, []int64{assetID})
	if toolErr != nil {
		t.Fatalf("tool surface gate: %v", toolErr)
	}
	if len(hits) != 1 {
		t.Fatalf("工具面判定应命中 1 条，实际 %d 条", len(hits))
	}
	if !strings.Contains(err.Error(), hits[0].Reason) {
		t.Fatalf("拒绝理由与工具面不一致：\n工具面: %s\nAPI  : %s", hits[0].Reason, err.Error())
	}
}

// TestGuardTaskAssetAttachRejectsOutOfScopeAsset 钉住范围注入视图判定：
// 范围外的资产挂不上 —— 即使用全局资产查询能查到它。
//
// 这条是「范围注入视图是安全前提，不是性能选择」的验收：若判定改用全局查询，
// 范围外的资产会混进判定，判定就等于没做。
func TestGuardTaskAssetAttachRejectsOutOfScopeAsset(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "out of scope")

	assetID := guardTestAsset(t, m, fmt.Sprintf("outofscope-%d.example.com", time.Now().UnixNano()))
	// 全局查询能找到它（它确实存在）。
	if _, err := as.GetByIDs([]int64{assetID}); err != nil {
		t.Fatalf("global asset lookup: %v", err)
	}
	// 但它不在任务的有效范围里。
	inScope, err := as.ScopedAssetIDs(taskID, []int64{assetID})
	if err != nil {
		t.Fatalf("scoped asset ids: %v", err)
	}
	if len(inScope) != 0 {
		t.Fatalf("前置条件不成立：资产竟在范围内 %v", inScope)
	}

	guardErr := s.guardTaskAssetAttach(as, taskID, []int64{assetID})
	if guardErr == nil {
		t.Fatal("范围外的资产不得挂载")
	}
	if !errors.Is(guardErr, errTaskAssetOutOfScope) {
		t.Fatalf("必须是范围外拒绝，实际: %v", guardErr)
	}
}

// TestGuardTaskAssetAttachAcceptsInScopeAsset 钉住 (a)：工具面会接受的请求，
// API 侧也接受 —— 资产已声明进任务范围时挂载成功。
func TestGuardTaskAssetAttachAcceptsInScopeAsset(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "in scope")

	assetID := guardTestAsset(t, m, fmt.Sprintf("inscope-%d.example.com", time.Now().UnixNano()))
	if err := m.pg.AddTaskScopeAsset(taskID, assetID); err != nil {
		t.Fatalf("declare task scope asset: %v", err)
	}
	if err := s.guardTaskAssetAttach(as, taskID, []int64{assetID}); err != nil {
		t.Fatalf("范围内的资产必须允许挂载: %v", err)
	}
}

// TestGuardTaskAssetAttachFailsClosedWhenScopeLookupFails 钉住 fail closed：
// 闸门规则 / 范围判定读不到 ⇒ 拒绝挂载。
func TestGuardTaskAssetAttachFailsClosedWhenScopeLookupFails(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "fail closed attach")

	if err := m.pg.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := s.guardTaskAssetAttach(as, taskID, []int64{1}); err == nil {
		t.Fatal("数据库故障时必须拒绝挂载（fail closed）")
	}
}

// TestGuardTaskScopeInputsAppliesAssetGate 钉住 scope 分支（登记范围规则）的
// 资产闸门：命中拦截规则的域名登记不上，且理由与工具面同一套 EvaluateAssetGate。
func TestGuardTaskScopeInputsAppliesAssetGate(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "scope inputs gate")

	blocked := fmt.Sprintf("blocked-scope-%d.example.com", time.Now().UnixNano())
	if _, err := m.pg.CreateAssetInterceptRule("exact_domain", blocked, "测试拦截", true); err != nil {
		t.Fatalf("create intercept rule: %v", err)
	}

	err := s.guardTaskScopeInputs(as, taskID, []db.ScopeInput{{Kind: "domain", Value: blocked}})
	if err == nil {
		t.Fatal("命中拦截规则的域名不得登记为任务范围")
	}
	if !errors.Is(err, errTaskAssetGateDenied) {
		t.Fatalf("必须是资产闸门拒绝，实际: %v", err)
	}
	// 工具面同一套闸门的理由必须一致。
	blockRules, ruleErr := as.ListAssetInterceptRules()
	if ruleErr != nil {
		t.Fatalf("list intercept rules: %v", ruleErr)
	}
	decision := db.EvaluateAssetGate(blockRules, nil, []string{blocked}, nil, nil)
	if decision.Allowed {
		t.Fatal("前置条件不成立：工具面闸门竟放行")
	}
	if !strings.Contains(err.Error(), decision.Reason) {
		t.Fatalf("拒绝理由与工具面不一致：\n工具面: %s\nAPI  : %s", decision.Reason, err.Error())
	}

	clean := fmt.Sprintf("clean-scope-%d.example.com", time.Now().UnixNano())
	if err := s.guardTaskScopeInputs(as, taskID, []db.ScopeInput{{Kind: "domain", Value: clean}}); err != nil {
		t.Fatalf("未被拦截的域名必须允许登记: %v", err)
	}
}

// TestAttachTaskAssetsHTTPRejectsBlockedAsset 走真实 HTTP handler 验一遍：
// 被闸门拒绝的资产一个都不能挂上。
func TestAttachTaskAssetsHTTPRejectsBlockedAsset(t *testing.T) {
	m := guardTestManager(t)
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "http attach")

	blocked := fmt.Sprintf("blocked-http-%d.example.com", time.Now().UnixNano())
	if _, err := m.pg.CreateAssetInterceptRule("exact_domain", blocked, "测试拦截", true); err != nil {
		t.Fatalf("create intercept rule: %v", err)
	}
	assetID := guardTestAsset(t, m, blocked)
	if err := m.pg.AddTaskScopeAsset(taskID, assetID); err != nil {
		t.Fatalf("declare task scope asset: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"asset_ids":      []int64{assetID},
		"source_summary": "操作者勾选",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/1/assets", bytes.NewReader(body))
	req.SetPathValue("id", i64s(taskID))
	rec := httptest.NewRecorder()
	s.attachTaskAssets(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("被拦截的资产应返回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "资产未通过任务测试范围校验") {
		t.Fatalf("拒绝理由不对：%s", rec.Body.String())
	}
	var links int
	if err := m.pg.QueryRow(`SELECT count(*) FROM task_asset_links WHERE task_id=$1 AND asset_id=$2`, taskID, assetID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Fatalf("被拒绝的挂载仍留下了 %d 条资产链接", links)
	}
}

// =====================================================================
// 端点四：POST/PUT/DELETE /api/tasks/{id}/intercept-rules(+toggle)
// =====================================================================

// TestGuardTaskInterceptRuleProtectsCommandLevelRules 钉住最锋利的那条边：
// 命令级（证据保全）规则是安全控制，API 侧不得新建 / 改写 / 删除 / 停用它。
func TestGuardTaskInterceptRuleProtectsCommandLevelRules(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "intercept protect")

	// 命令级规则由平台的证据保全策略安装（唯一入口：db.AddTaskInterceptRule）。
	if err := as.AddTaskInterceptRule(taskID, "[任务级·证据保全] 改写日志内容", `(?i)\bsed\b`, "禁止改写日志"); err != nil {
		t.Fatalf("install command rule: %v", err)
	}
	rules, err := as.ListTaskInterceptRules(taskID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("前置条件不成立：命令级规则 %d 条", len(rules))
	}
	commandRule := rules[0]
	if commandRule.Kind != db.TaskCommandRuleKind {
		t.Fatalf("前置条件不成立：规则 kind=%q", commandRule.Kind)
	}

	// 新建命令级规则 ⇒ 拒绝。
	createErr := guardTaskInterceptRuleCreate(&taskInterceptRuleReq{Kind: db.TaskCommandRuleKind, Pattern: `(?i)rm`})
	if !errors.Is(createErr, errTaskCommandRuleProtected) {
		t.Fatalf("新建命令级规则必须被拒，实际: %v", createErr)
	}

	// 改写命令级规则 ⇒ 拒绝。
	if err := s.guardTaskInterceptRuleMutate(as, taskID, commandRule.ID); !errors.Is(err, errTaskCommandRuleProtected) {
		t.Fatalf("改写命令级规则必须被拒，实际: %v", err)
	}

	// 停用命令级规则 ⇒ 拒绝；启用（加固方向）⇒ 放行。
	if err := s.guardTaskInterceptRuleToggle(as, taskID, commandRule.ID, false); !errors.Is(err, errTaskCommandRuleProtected) {
		t.Fatalf("停用命令级规则必须被拒，实际: %v", err)
	}
	if err := s.guardTaskInterceptRuleToggle(as, taskID, commandRule.ID, true); err != nil {
		t.Fatalf("启用命令级规则（加固）必须放行: %v", err)
	}

	// 规则必须还在（没被任何一条路径动过）。
	after, err := as.ListTaskInterceptRules(taskID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(after) != 1 || after[0].ID != commandRule.ID || !after[0].Enabled {
		t.Fatalf("命令级规则被改动：%+v", after)
	}
}

// TestGuardTaskInterceptRuleAllowsAssetLevelRules 钉住另一侧：资产级规则是操作者
// 自己的测试范围声明，改写/删除照常放行（判定不能把操作者的正常功能一起锁死）。
func TestGuardTaskInterceptRuleAllowsAssetLevelRules(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "intercept asset level")

	rule, err := as.CreateTaskInterceptRule(taskID, "block", "exact_domain", "example.com", "操作者自己加的", true)
	if err != nil {
		t.Fatalf("create asset rule: %v", err)
	}
	if err := s.guardTaskInterceptRuleMutate(as, taskID, rule.ID); err != nil {
		t.Fatalf("资产级规则的改写必须放行: %v", err)
	}
	if err := s.guardTaskInterceptRuleToggle(as, taskID, rule.ID, false); err != nil {
		t.Fatalf("资产级规则的停用必须放行: %v", err)
	}
}

// TestGuardTaskInterceptRuleFailsClosedWhenRuleLookupFails 钉住 fail closed：
// 规则读不到 ⇒ 拒绝改动（不能因为「查不到这条规则是什么」就放行）。
func TestGuardTaskInterceptRuleFailsClosedWhenRuleLookupFails(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "intercept fail closed")

	if err := m.pg.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := s.guardTaskInterceptRuleMutate(as, taskID, 1); err == nil {
		t.Fatal("数据库故障时必须拒绝改写（fail closed）")
	}
	if err := s.guardTaskInterceptRuleToggle(as, taskID, 1, false); err == nil {
		t.Fatal("数据库故障时必须拒绝停用（fail closed）")
	}
}

// TestTaskInterceptRuleHTTPMutationRejected 走真实 HTTP handler 验一遍：
// 命令级规则经 API 改不动。
func TestTaskInterceptRuleHTTPMutationRejected(t *testing.T) {
	m := guardTestManager(t)
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "intercept http")

	if err := m.pg.Assets().AddTaskInterceptRule(taskID, "[任务级·证据保全] 清理日志", `(?i)\bhistory\s+-c\b`, "禁止清历史"); err != nil {
		t.Fatalf("install command rule: %v", err)
	}
	rules, err := m.pg.Assets().ListTaskInterceptRules(taskID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	ruleID := rules[0].ID

	// 新建命令级规则 ⇒ 400。
	createBody, err := json.Marshal(map[string]any{
		"enabled": true, "action": "block", "kind": db.TaskCommandRuleKind, "pattern": `(?i)rm`,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/1/intercept-rules", bytes.NewReader(createBody))
	req.SetPathValue("id", i64s(taskID))
	rec := httptest.NewRecorder()
	s.taskInterceptCreateRule(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("新建命令级规则应返回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// 删除命令级规则 ⇒ 400（且规则必须还在）。
	delReq := httptest.NewRequest(http.MethodDelete, "/api/tasks/1/intercept-rules/1", nil)
	delReq.SetPathValue("id", i64s(taskID))
	delReq.SetPathValue("rid", i64s(ruleID))
	delRec := httptest.NewRecorder()
	s.taskInterceptDeleteRule(delRec, delReq)
	if delRec.Code != http.StatusBadRequest {
		t.Fatalf("删除命令级规则应返回 400，实际 %d：%s", delRec.Code, delRec.Body.String())
	}

	// 停用命令级规则 ⇒ 400。
	toggleBody, err := json.Marshal(map[string]any{"enabled": false})
	if err != nil {
		t.Fatal(err)
	}
	toggleReq := httptest.NewRequest(http.MethodPost, "/api/tasks/1/intercept-rules/1/toggle", bytes.NewReader(toggleBody))
	toggleReq.SetPathValue("id", i64s(taskID))
	toggleReq.SetPathValue("rid", i64s(ruleID))
	toggleRec := httptest.NewRecorder()
	s.taskInterceptToggleRule(toggleRec, toggleReq)
	if toggleRec.Code != http.StatusBadRequest {
		t.Fatalf("停用命令级规则应返回 400，实际 %d：%s", toggleRec.Code, toggleRec.Body.String())
	}

	after, err := m.pg.Assets().ListTaskInterceptRules(taskID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(after) != 1 || !after[0].Enabled {
		t.Fatalf("命令级规则被 API 改动了：%+v", after)
	}
}

// =====================================================================
// 操作者侧 vs agent 侧的区分
// =====================================================================

// TestOperatorScopeDeclarationIsNotCappedAndDoesNotConsumeAgentBudget 钉住区分：
//
//   - 操作者侧声明（db.AddTaskScopeAsset / db.RegisterTaskAssetScopes）不受 agent
//     扩权上限约束 —— 人工设范围是授权决定本身。
//   - 操作者侧声明不消耗 agent 的扩张预算 —— 否则人工设一次范围就把 agent 的
//     额度吃光了。
//   - agent 侧通道（add_task_scope / API 闸门）两者都受约束。
func TestOperatorScopeDeclarationIsNotCappedAndDoesNotConsumeAgentBudget(t *testing.T) {
	m := guardTestManager(t)
	as := m.pg.Assets()
	s := &Server{m: m}
	taskID := guardTestTask(t, m, "operator vs agent")

	// 操作者侧：声明 12 个资产（远超 agent 的扩权上限 8 条）—— 全部成功。
	for i := 0; i < 12; i++ {
		assetID := guardTestAsset(t, m, fmt.Sprintf("operator-%d-%d.example.com", time.Now().UnixNano(), i))
		if err := m.pg.AddTaskScopeAsset(taskID, assetID); err != nil {
			t.Fatalf("操作者侧资产声明第 %d 条被拒（不该受 agent 上限约束）: %v", i+1, err)
		}
	}

	// 操作者侧：声明根域范围（agent 侧被拒的 kind）—— 仍然成功。
	if _, err := as.RegisterTaskAssetScopes(taskID, []db.ScopeInput{{Kind: "domain", Value: "operator-root.example.com"}}); err != nil {
		t.Fatalf("操作者侧根域范围声明被拒（不该受 agent kind 白名单约束）: %v", err)
	}
	rows := guardScopeRows(t, m, taskID)
	manualRows := 0
	for _, row := range rows {
		if row.Source == "manual" {
			manualRows++
		}
	}
	if manualRows != 13 {
		t.Fatalf("操作者侧声明应落 13 行 manual 来源，实际 %d 行（共 %d 行）", manualRows, len(rows))
	}

	// agent 侧：预算没被操作者吃掉，仍能加满上限。
	cap := db.AgentScopeExpansionCap()
	for i := 0; i < cap; i++ {
		domain := fmt.Sprintf("agent-%d-%d.example.com", time.Now().UnixNano(), i)
		if _, err := s.guardTaskScopeAdd(as, taskID, taskScopeAddRequest{Kind: "subdomain", Value: domain, Reason: "理由"}); err != nil {
			t.Fatalf("agent 侧第 %d 条被拒（操作者的声明不该占 agent 预算）: %v", i+1, err)
		}
	}
	// 再加一条 ⇒ 上限拒绝。
	if _, err := s.guardTaskScopeAdd(as, taskID, taskScopeAddRequest{Kind: "subdomain", Value: "agent-over.example.com", Reason: "理由"}); err == nil {
		t.Fatal("agent 侧超出上限必须被拒")
	}

	// agent 侧：kind 白名单仍生效（操作者侧能声明 root_domain，agent 侧不能）。
	if _, err := s.guardTaskScopeAdd(as, taskID, taskScopeAddRequest{Kind: "root_domain", Value: "agent-root.example.com", Reason: "理由"}); err == nil {
		t.Fatal("agent 侧不得自行把范围扩到 root_domain")
	}
}
