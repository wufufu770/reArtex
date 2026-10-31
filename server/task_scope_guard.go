package server

// 本文件是【平台 API 侧写路径的判定闸门】—— 通道一致性的落点。
//
// 为什么需要它：权限判定此前只发生在【工具面】（agent 的工具调用），不发生在
// 【副作用发生处】。同一个副作用换个通道发生，判定就漂移了：工具面拒绝的请求，
// 换成 curl 调本机 API 就通过了。
//
// 覆盖的入口（研究笔记 §35 逐条核对路由表后实测出来的、真正能扩权的 API 写入口）：
//
//	POST   /api/tasks/{id}/scope                        → 工具面 add_task_scope
//	DELETE /api/tasks/{id}/scope/{sid}                  → 工具面 add_task_scope（删除侧）
//	POST   /api/tasks/{id}/assets                       → 工具面 insert_assets
//	POST/PUT/DELETE /api/tasks/{id}/intercept-rules(+/toggle) → 工具面无写面
//
// 三条纪律：
//
//  1. **复用工具面的判定代码路径**，不另写一份 —— 两份实现会漂移，比只有一份更坏。
//     能直接调工具面用的那个函数就调它（AddAgentScope / CheckAssetsIntercept /
//     EvaluateAssetGate / ScopedAssetIDs），判定语义与拒绝理由因此逐字一致。
//  2. **判定失败一律拒绝**（fail closed）：范围查不到、规则读不到、策略读不到，
//     都必须拒绝，绝不「出错就放行」。
//  3. **操作者侧与 agent 侧必须分得开**（详见下方各闸门的注释）：
//     操作者声明走 AddManualScope / AddTaskScopeAsset（落库 source='manual'，
//     不受 agent 扩权上限约束）；agent 侧走 AddAgentScope（source='agent'，
//     受 kind 白名单 + 扩权条数上限约束）。两者在代码与数据上都可分辨。
import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// =====================================================================
// 1. 任务范围写入：POST /api/tasks/{id}/scope
// =====================================================================

// taskScopeAddRequest 是「给任务加一条范围」的入参。
//
// 为什么打包成一个类型：kind/value/reason 不是三个独立输入，而是同一条范围声明
// 的三个字段（工具面 add_task_scope 的 entries 元素也是这个形状）；拆成三个
// 位置参数会让调用点与工具面的调用点长得不一样，而「长得一样」正是这里要的。
type taskScopeAddRequest struct {
	Kind   string
	Value  string
	Reason string
}

// guardTaskScopeAdd 是「给任务加一条范围」的判定闸门。
//
// **判定路径直接复用工具面**：agent/tools_insert.go 的 add_task_scope 调的就是
// db.(*AssetStore).AddAgentScope，这里调的是同一个函数 ⇒ 四项逐字一致：
//
//   - kind 白名单（scopepolicy.Policy.MaxAgentAddedKinds：不含 root_domain/company）
//   - 扩权条数上限（db.AgentScopeExpansionCap()）
//   - 拒绝理由（*db.ScopeExpansionDeniedError.Reason，原样中文，不二次包装）
//   - 拒绝留痕（审计链 scope_expansion_denied 事件）
//
// **operator vs agent**：这条入口判定为 **agent 侧**通道。
//
// 为什么不是操作者侧：这条 API 是工具面 add_task_scope 的孪生入口（kind/value/reason
// 三个字段语义完全一致），而 db/task_scope.go 的注释把「agent 借『人工』名义绕过
// 上限」点名为既有风险 —— API 侧无法证明调用者是操作者（单一 JWT、无角色区分），
// 却能拿到「人工路径」的口径 ⇒ 它必须按 agent 侧判定。
//
// 操作者声明任务范围走另外两条【操作者侧】通道（落库 source='manual'，不受 agent
// 扩权上限约束，与 db.AddTaskScopeAsset 的口径一致）：
//
//   - POST /api/tasks/{id}/assets（scope 分支）→ db.RegisterTaskAssetScopes
//   - 任务大厅发布（POST /api/task-hall/tasks）→ db.AddTaskScopeAsset
//
// **失败即拒绝**：判定失败一律拒绝（不放行）。拒绝理由【原样透出】，不二次包装
// —— 包装会改写工具面的拒绝理由，而「同一句理由」正是通道一致性要保证的东西。
// （代价：底层数据库故障的原始错误文本也会原样透出；这与工具面把 err.Error()
// 原样回给模型是同一口径。）
func (s *Server) guardTaskScopeAdd(as *db.AssetStore, taskID int64, req taskScopeAddRequest) (db.TaskScope, error) {
	if as == nil {
		return db.TaskScope{}, errors.New("资产库未启用")
	}
	// 参数形态与工具面完全一致（kind 去空白、value/reason 原样传入）。
	return as.AddAgentScope(taskID, strings.TrimSpace(req.Kind), req.Value, req.Reason, "agent")
}

// =====================================================================
// 2. 任务范围删除：DELETE /api/tasks/{id}/scope/{sid}
// =====================================================================

// guardTaskScopeDelete 是「删一条任务范围行」的判定闸门。
//
// 删除是**收窄**范围的动作（不产生扩权），但它有两处必须判：
//
//   - **归属判定**：要删的行必须属于【本任务自己的】范围集合。继承自来源任务的行
//     （scopeTargetCTE 的 context_tasks 口径）与别的任务的行都不属于本任务，
//     删它们等于跨任务改授权边界。判定走 db.ListTaskScope —— 它只取
//     `WHERE ts.task_id=$1`，即本任务自己的行。
//   - **fail closed**：归属查询失败 ⇒ 拒绝（不是「查不到就当可以删」）。
//
// 真正的删除仍走 db.DeleteTaskScope —— 它的 SQL 自带 `AND task_id=$2`，
// 与归属判定形成双保险：即便归属判定被绕过，删除也碰不到别的任务的行。
//
// **operator vs agent**：操作者侧（收窄动作，不吃 agent 的扩权预算，也不放宽
// 任何授权边界），但不允许跨任务删除。
func (s *Server) guardTaskScopeDelete(as *db.AssetStore, taskID, scopeID int64) (bool, error) {
	if as == nil {
		return false, errors.New("资产库未启用")
	}
	rows, err := as.ListTaskScope(taskID)
	if err != nil {
		// 归属判定读不到 ⇒ fail closed：拒绝删除。
		return false, fmt.Errorf("查询任务范围行失败，已拒绝删除: %w", err)
	}
	owned := false
	for _, row := range rows {
		if row.ID == scopeID {
			owned = true
			break
		}
	}
	if !owned {
		// 不是本任务自己的行（含继承自来源任务的行）⇒ 不删。
		return false, nil
	}
	return as.DeleteTaskScope(taskID, scopeID)
}

// =====================================================================
// 3. 任务资产挂载：POST /api/tasks/{id}/assets
// =====================================================================

// errTaskAssetGateDenied 标记「命中资产闸门」—— 与工具面同一套闸门判定拒绝。
var errTaskAssetGateDenied = errors.New("资产未通过任务测试范围校验")

// errTaskAssetOutOfScope 标记「资产不在任务的有效范围内」。
var errTaskAssetOutOfScope = errors.New("资产不在本任务的有效范围内")

// guardTaskAssetAttach 是「把资产挂到任务上」的判定闸门。
//
// **判定一：资产闸门（先拦截后允许）** —— 与工具面 insert_assets 同一套规则。
// 复用 db.(*AssetStore).CheckAssetsIntercept（agent/tools.go 的意图锚点校验用的
// 就是它，内部走 db.EvaluateAssetGate）；拒绝理由沿用同一条命中原因。
//
// **判定二：范围注入视图** —— 挂上去的资产必须落在【本任务的有效范围】里。
//
// 为什么这条必须有：assets.task_ids 是「链接视图」，task_scope 才是 agent 的
// 「范围视图」（scopeTargetCTE 只读 task_scope）。挂一个范围外的资产进来，
// 会让这个任务多出一个可达目标（db.HostsByTaskWithSources 读 task_ids），
// 却不在它声明的授权边界内 —— 这正是「挂资产」这条通道能扩权的原因。
//
// 为什么必须用范围注入视图（db.ScopedAssetIDs，内部走 db.ScopeFragmentFor）
// 而不是全局资产查询：全局查询会让**范围外**的资产混进判定，判定就等于没做。
// 这是安全前提，不是性能选择。
//
// **operator vs agent**：这条入口是【操作者侧】通道（它的库侧对应物是
// db.AddTaskScopeAsset / db.RegisterTaskAssetScopes，落库 source='manual'，
// 按设计不受 agent 扩权上限约束），但**不跳过**上面两道判定。
func (s *Server) guardTaskAssetAttach(as *db.AssetStore, taskID int64, assetIDs []int64) error {
	if as == nil {
		return errors.New("资产库未启用")
	}
	ids := make([]int64, 0, len(assetIDs))
	seen := make(map[int64]struct{}, len(assetIDs))
	for _, id := range assetIDs {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	// 判定一：资产闸门。
	hits, err := as.CheckAssetsIntercept(taskID, ids)
	if err != nil {
		// 规则读不到 ⇒ fail closed：拒绝挂载。
		return fmt.Errorf("%w：读取拦截规则失败，已拒绝挂载: %w", errTaskAssetGateDenied, err)
	}
	if len(hits) > 0 {
		var b strings.Builder
		for _, hit := range hits {
			fmt.Fprintf(&b, "\n - %s", hit.Describe())
		}
		return fmt.Errorf("%w，已拒绝挂载：%s", errTaskAssetGateDenied, b.String())
	}
	// 判定二：范围注入视图。
	inScope, err := as.ScopedAssetIDs(taskID, ids)
	if err != nil {
		// 范围判定失败 ⇒ fail closed：拒绝挂载。
		return fmt.Errorf("%w：范围判定失败，已拒绝挂载: %w", errTaskAssetOutOfScope, err)
	}
	if len(inScope) != len(ids) {
		return fmt.Errorf("%w：%d 个资产不在本任务的有效范围内，已拒绝挂载（挂载会把范围外的目标带进任务）",
			errTaskAssetOutOfScope, len(ids)-len(inScope))
	}
	return nil
}

// guardTaskScopeInputs 是「登记一批范围规则到任务」（POST /api/tasks/{id}/assets 的
// scope 分支）的判定闸门。
//
// 与工具面 insert_assets 同一套闸门：判定发生在资产真正落库之前，被拒的资产
// 禁止插入。判定函数复用 db.EvaluateAssetGate（工具面用的同一个）。
//
// 规则集口径与工具面一致：拦截规则 = 全局 ∪ 任务级 block；允许规则 = 任务级 allow。
//
// 候选串的取法与工具面一致（agent/tools_insert.go 的 assetInterceptCandidates）：
// 域名进 domains、IP 进 ips、URL 进 urls。这里登记的范围规则只会产生域名 / IP
// 资产（icp / keyword 不产生资产），因此没有 urls。
func (s *Server) guardTaskScopeInputs(as *db.AssetStore, taskID int64, inputs []db.ScopeInput) error {
	if as == nil {
		return errors.New("资产库未启用")
	}
	blockRules, err := as.ListAssetInterceptRules()
	if err != nil {
		return fmt.Errorf("%w：读取拦截规则失败，已拒绝登记范围: %w", errTaskAssetGateDenied, err)
	}
	var allowRules []db.AssetInterceptRule
	if taskID > 0 {
		taskBlock, taskAllow, err := as.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return fmt.Errorf("%w：读取任务拦截规则失败，已拒绝登记范围: %w", errTaskAssetGateDenied, err)
		}
		blockRules = append(blockRules, taskBlock...)
		allowRules = taskAllow
	}

	var domains, ips []string
	for i, input := range inputs {
		parsed, err := db.ParseScopeInput(input)
		if err != nil {
			return fmt.Errorf("%w：第 %d 条范围无效: %v", errTaskAssetGateDenied, i+1, err)
		}
		switch parsed.Kind {
		case "domain":
			if parsed.Domain != "" {
				domains = append(domains, parsed.Domain)
			}
		case "ip":
			// 候选必须是裸 IP：exact_ip / fuzzy_ip 规则按字面匹配，
			// 传 "1.2.3.4/32" 会让精确匹配失效（判定被悄悄削弱）。
			if ip := bareIP(parsed.Net); ip != "" {
				ips = append(ips, ip)
			}
		case "cidr":
			// 网段本身不是 IP 字面量，闸门按 IP 候选匹配 ⇒ 取网段起始地址做候选，
			// 落在被拦截网段内的网段照样被拒（保守方向：宁可多拒）。
			if ip := bareIP(parsed.Net); ip != "" {
				ips = append(ips, ip)
			}
		case "icp", "keyword":
			// 这两种范围不产生资产 ⇒ 没有闸门候选（与工具面 insert_assets 的
			// assetInputCandidates 同口径：没有资产就没有可拦截的目标）。
		}
	}
	if decision := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, nil); !decision.Allowed {
		return fmt.Errorf("%w：%s，已拒绝登记范围", errTaskAssetGateDenied, decision.Reason)
	}
	return nil
}

// bareIP 把 CIDR 文本还原成裸 IP（"1.2.3.4/32" → "1.2.3.4"）。
// 解析不出来返回空串。
func bareIP(netText string) string {
	text := strings.TrimSpace(netText)
	if text == "" {
		return ""
	}
	if ip, _, err := net.ParseCIDR(text); err == nil {
		return ip.String()
	}
	if ip := net.ParseIP(text); ip != nil {
		return ip.String()
	}
	return ""
}

// =====================================================================
// 4. 任务级拦截规则：POST/PUT/DELETE /api/tasks/{id}/intercept-rules(+toggle)
// =====================================================================

var (
	// errTaskCommandRuleProtected：命令级拦截规则是任务的安全控制本身。
	//
	// 证据保全策略装的命令级规则（db.TaskCommandRuleKind = "command_regex"）拦的是
	// 「改日志 / 清历史 / 抽 journal」这类会毁掉取证证据的工具调用。工具面**没有**
	// 任何写面能改它们 ⇒ 按通道一致性，API 侧也不能提供更弱的写面：
	// 改写 / 停用 / 删除一条命令级规则 = 拆掉安全控制。
	errTaskCommandRuleProtected = errors.New("命令级拦截规则是任务的安全控制（证据保全策略），不能通过接口新建、改写或删除")
	// errTaskInterceptRuleNotFound：目标任务下没有这条规则。
	errTaskInterceptRuleNotFound = errors.New("任务级拦截规则不存在")
)

// guardTaskInterceptRuleCreate 判定「新建一条任务级拦截规则」是否允许。
//
// 命令级规则（command_regex）只能由平台的证据保全策略安装
// （db.AddTaskInterceptRule —— 它会先编译正则，装不上就不装：一条编译不过的
// 正则会被拦截器静默跳过，「装上了却不生效」比没装更坏）。因此通用规则 CRUD
// 不接受命令级 kind。
func guardTaskInterceptRuleCreate(req *taskInterceptRuleReq) error {
	if strings.TrimSpace(req.Kind) == db.TaskCommandRuleKind {
		return errTaskCommandRuleProtected
	}
	return nil
}

// guardTaskInterceptRuleMutate 判定「改写 / 删除一条任务级拦截规则」是否允许。
//
// 判定的对象是【规则本身是不是安全控制】：命令级规则一律拒绝改写与删除
// （改写可以顺手把 kind 改成资产级、可以停用它、可以换掉它的正则 —— 三条都是
// 拆安全控制）。资产级规则（域名/IP/URL/CIDR 的拦截与允许）是操作者自己的
// 测试范围声明，放行。
//
// fail closed：规则读不到 ⇒ 拒绝（不能因为「查不到这条规则是什么」就放行改动）。
func (s *Server) guardTaskInterceptRuleMutate(as *db.AssetStore, taskID, ruleID int64) error {
	rule, found, err := findTaskInterceptRule(as, taskID, ruleID)
	if err != nil {
		return fmt.Errorf("读取任务拦截规则失败，已拒绝修改: %w", err)
	}
	if !found {
		return errTaskInterceptRuleNotFound
	}
	if rule.Kind == db.TaskCommandRuleKind {
		return errTaskCommandRuleProtected
	}
	return nil
}

// guardTaskInterceptRuleToggle 判定「启用 / 停用一条任务级拦截规则」是否允许。
//
// 停用命令级规则 = 拆安全控制 ⇒ 拒绝；启用（加固方向）放行。
// 资产级规则的启停是操作者自己的范围声明，放行。
func (s *Server) guardTaskInterceptRuleToggle(as *db.AssetStore, taskID, ruleID int64, enabled bool) error {
	if enabled {
		// 启用只会让规则更严 ⇒ 只校验规则归属（不存在的规则没有可启用的对象）。
		_, found, err := findTaskInterceptRule(as, taskID, ruleID)
		if err != nil {
			return fmt.Errorf("读取任务拦截规则失败，已拒绝修改: %w", err)
		}
		if !found {
			return errTaskInterceptRuleNotFound
		}
		return nil
	}
	return s.guardTaskInterceptRuleMutate(as, taskID, ruleID)
}

// findTaskInterceptRule 在任务的规则集合里按 id 找一条规则。
//
// 走的是列表接口那一条读路径（db.ListTaskInterceptRules）：它按 task_id 取，
// 因此别的任务的规则天然查不到 ⇒ 跨任务改规则在这里就被挡住。
func findTaskInterceptRule(as *db.AssetStore, taskID, ruleID int64) (db.AssetInterceptRule, bool, error) {
	if as == nil {
		return db.AssetInterceptRule{}, false, errors.New("资产库未启用")
	}
	rules, err := as.ListTaskInterceptRules(taskID)
	if err != nil {
		return db.AssetInterceptRule{}, false, err
	}
	for _, rule := range rules {
		if rule.ID == ruleID {
			return rule, true, nil
		}
	}
	return db.AssetInterceptRule{}, false, nil
}
