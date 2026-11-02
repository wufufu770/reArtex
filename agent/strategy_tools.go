package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件是策略库（L4）面向 agent 的三个工具：检索、按需加载、提议。
//
// 三条硬约束（都是结构性的，不是约定俗成的）：
//
//  1. **检索只返 active**：quarantined（未经验证的新策略）/ retired / tombstone
//     一律不进检索结果 —— 访问层（db.SearchActiveStrategies）的 SQL 已经按 state
//     过滤，本工具只消费它的结果，不自己放宽。
//  2. **检索结果只带摘要**：steps 是命令模板，把它放进上下文等于「把可执行产物塞进
//     上下文」（反模式 8），而本阶段**没有**任何执行前的拦截关口。
//  3. **提议写不出 active**：CreateStrategy 结构上只允许 quarantined，晋级是裁决写，
//     只在宿主侧发生（auto-permission-tiers §4.2）。
//
// 注入路径（planner 的初始上下文）与本文件是两条路：注入只带摘要（见
// RenderStrategySummaries 与 planner.go 的 SetStrategyContextProvider），
// 正文按需用 strategy_get 单独加载（那个工具默认不绑给任何 agent）。
//
// allow: SIZE_OK —— 本单元的文件域被计划钉死为策略库面向 agent 的三个工具 +
// 摘要渲染器：检索 / 按需加载 / 提议共用同一套「资产签名 + 只返摘要 + 提议写
// 不出裁决态」的契约，拆开会把一份契约切成三处。

// strategySearch 让 agent 检索可复用的策略。
//
// 只返 state='active'：quarantined 是隔离态（未经验证的新策略），retired /
// tombstone 是历史 —— 三者都不进检索注入。
//
// 硬顶 top-k（≤ db.StrategySearchTopK）+ 相关性阈值 + 只返摘要（反模式 7/8）。
//
// ⚠️ 候选集是【结构路径 ∪ 字面路径】：结构命中（资产类型 / 服务指纹 / 技术栈标签）
// 的策略**即使字面相似度为 0 也会返回**。query 只影响**字面路径**这一路，
// 不影响结构路径。
func (t *ToolSet) strategySearch() actool.CoreTool {
	return t.readExpTool("strategy_search", "检索可复用的策略（只返回已激活的策略摘要）。",
		obj(map[string]any{
			"query": str("可选：关键词，用于在摘要里做字面匹配（只影响字面路径，不影响结构匹配）"),
			"limit": intp("返回条数上限（默认 10，最大 10）"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return t.strategySearchResult(in)
		})
}

// strategySearchResult 执行检索并渲染成摘要列表。
func (t *ToolSet) strategySearchResult(in json.RawMessage) (actool.Result, error) {
	var args struct {
		Query string `json:"query"`
		Limit int64  `json:"limit"`
	}
	if len(in) > 0 {
		_ = json.Unmarshal(in, &args)
	}
	limit := int(args.Limit)
	if limit <= 0 || limit > db.StrategySearchTopK {
		limit = db.StrategySearchTopK
	}
	store := t.strategyDB()
	if store == nil {
		return actool.Errorf("策略库不可用：当前工具集没有数据库句柄"), nil
	}
	// 资产签名必须来自 scope 注入版查询。没有它 ⇒ 签名是空的 ⇒ 结构路径要么
	// 匹配不到任何东西、要么退化成「匹配一切」⇒ 宁可拒绝检索，也不给一个没有
	// 范围边界的策略匹配。
	if t.as == nil {
		return actool.Errorf("缺少资产范围上下文，拒绝检索策略（策略匹配必须基于范围内资产的签名）"), nil
	}
	taskID := t.scopeTaskID()
	if taskID <= 0 {
		return actool.Errorf("缺少任务上下文，拒绝检索策略"), nil
	}
	sig, err := t.as.TaskAssetSignature(taskID)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("算任务资产签名失败: %v", err)), nil
	}
	matches, err := store.SearchActiveStrategies(taskID, sig, limit, db.StrategyMinRelevance)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("检索策略失败: %v", err)), nil
	}
	matches = strategySearchKeeps(matches, args.Query)
	items := make([]map[string]any, 0, len(matches))
	for _, m := range matches {
		// 只输出摘要字段。**不输出 steps / activation / preconditions**：
		// 正文不进检索结果（反模式 8）。
		items = append(items, map[string]any{
			"id":      m.ID,
			"name":    m.Name,
			"summary": m.Summary,
			"utility": m.Utility,
			"version": m.Version,
		})
	}
	return jsonResult(map[string]any{"strategies": items, "count": len(items)})
}

// strategyGet 按需加载一条策略的完整内容。
//
// ⚠️ 本阶段**不执行**策略：正文只作参考文本返回，没有渲染、没有执行路径。
// 将来落地执行时，每条渲染出的命令必须逐条过既有 20 条内置规则与
// intercept.Interceptor.Match（design §6）。
//
// 这个工具**默认不绑给任何 agent**（defaultUnbound）：它把跨任务知识拉进上下文，
// 风险高于检索摘要，要用就人工 opt-in。
func (t *ToolSet) strategyGet() actool.CoreTool {
	return t.readExpTool("strategy_get", "按 id 加载一条策略的完整内容（正文按需加载，不进检索结果）。",
		obj(map[string]any{"id": intp("策略 id")}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return t.strategyGetResult(in)
		})
}

// strategyGetResult 读一条策略的正文。
func (t *ToolSet) strategyGetResult(in json.RawMessage) (actool.Result, error) {
	var args struct {
		ID int64 `json:"id"`
	}
	if len(in) > 0 {
		_ = json.Unmarshal(in, &args)
	}
	if args.ID <= 0 {
		return actool.Errorf("策略 id 不合法"), nil
	}
	store := t.strategyDB()
	if store == nil {
		return actool.Errorf("策略库不可用：当前工具集没有数据库句柄"), nil
	}
	s, err := store.GetStrategy(args.ID)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("读策略失败: %v", err)), nil
	}
	if s == nil {
		return actool.Errorf("策略不存在"), nil
	}
	// 只给 active：quarantined 只能被 curator 查询（人工复核与重放试验），
	// retired / tombstone 是历史，不该被 agent 当成「现在该怎么做」。
	if s.State != db.StrategyActive {
		return actool.Errorf(fmt.Sprintf("策略当前状态为 %s，只有已激活（active）的策略可被读取", s.State)), nil
	}
	return jsonResult(map[string]any{
		"id":               s.ID,
		"name":             s.Name,
		"summary":          s.Summary,
		"activation":       s.Activation,
		"steps":            s.Steps,
		"preconditions":    s.Preconditions,
		"tool_requirements": s.ToolRequirements,
		"success_criteria": s.SuccessCriteria,
		"failure_modes":    s.FailureModes,
		"parameters":       s.Parameters,
		"utility":          s.Utility,
		"version":          s.Version,
		"note":             "本阶段不执行策略：以上正文只是参考文本，没有渲染、没有执行路径。",
	})
}

// strategyPropose 让 agent 提议一条新策略。
//
// 只写：一条 state='quarantined' 的策略 + 一条 status='pending' 的提议。
// 写不出 active —— CreateStrategy 结构上就只允许 quarantined。
func (t *ToolSet) strategyPropose() actool.CoreTool {
	return t.writeExpTool(strategyProposalToolKey, "提议一条可复用的策略。**只提议**：提议会进隔离态，需要 curator 复核并过重放闸门才会生效。",
		obj(map[string]any{
			"name":     str("策略短名（≤64 字符）"),
			"summary":  str("一句话描述（≤1024 字符）"),
			"reason":   str("提议理由（为什么这条值得沉淀）"),
			"activation": map[string]any{
				"type":        "object",
				"description": "激活条件（资产图谓词：asset_type / service_type / category / record_type）",
			},
			"steps": map[string]any{
				"type":        "array",
				"description": "步骤模板（参数化，$TARGET/$PATH；正文里不得出现真实目标地址）",
			},
			"success_criteria": map[string]any{
				"type":        "array",
				"description": "里程碑（客观、可机器判定）",
			},
			"evidence": map[string]any{
				"type":        "array",
				"description": "证据引用（每项：type=finding|traffic_snapshot，id，digest）",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":   map[string]any{"type": "string", "description": "finding | traffic_snapshot"},
						"id":     map[string]any{"type": "string", "description": "证据 id"},
						"digest": map[string]any{"type": "string", "description": "内容摘要（可选）"},
					},
					"required": []string{"type", "id"},
				},
			},
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return t.strategyProposeResult(in)
		})
}

// strategyProposeResult 建隔离态策略 + 记证据 + 记待裁决提议。
func (t *ToolSet) strategyProposeResult(in json.RawMessage) (actool.Result, error) {
	var args struct {
		Name            string          `json:"name"`
		Summary         string          `json:"summary"`
		Reason          string          `json:"reason"`
		Activation      json.RawMessage `json:"activation"`
		Steps           json.RawMessage `json:"steps"`
		SuccessCriteria json.RawMessage `json:"success_criteria"`
		Evidence        json.RawMessage `json:"evidence"`
	}
	if len(in) > 0 {
		_ = json.Unmarshal(in, &args)
	}
	// 守卫：提议级写入必须真的只写隔离态记录（不声明状态、不改归属、不自证裁决）。
	// 判 Tier 3 ⇒ 拒绝写入，一个字节都不落库（设计 §4.2：裁决写只在宿主侧发生）。
	//
	// 为什么这道门在写入路径上而不是只在分层判定器里：判定器是纯函数，看不到
	// 这次写到底写了什么；「入参里带了一个 state 字段」这件事只有这里判得了。
	if verdict := guardStrategyProposal(strategyProposalToolKey, in); verdict.Tier != Tier2 {
		return actool.Errorf(verdict.Reason), nil
	}
	args.Name = strings.TrimSpace(args.Name)
	args.Summary = strings.TrimSpace(args.Summary)
	if args.Name == "" {
		return actool.Errorf("策略必须有名字"), nil
	}
	if args.Summary == "" {
		return actool.Errorf("策略必须有一句话描述"), nil
	}
	store := t.strategyDB()
	if store == nil {
		return actool.Errorf("策略库不可用：当前工具集没有数据库句柄"), nil
	}
	// 任务上下文是硬要求：提议必须能说清「这条策略是从哪个任务沉淀出来的」。
	//
	// 为什么不能只靠 needExploration 的 nil-store 守卫：那个守卫判的是「有没有
	// 探索图句柄」，而这里判的是「这条探索图是不是属于某个任务」。一份挂在
	// **不属于任何任务**的探索图上的工具集能通过前者，却没有授权边界 ——
	// 而 strategy_proposals 表没有 task_id 列，落库之后没人说得清它的出处。
	//
	// 与 strategy_search 同一把尺子（scopeTaskID）：两者都读任务内资产的
	// 签名/归属，缺任务上下文时宁可拒绝，也不给一个没有归属的提议。
	if taskID := t.scopeTaskID(); taskID <= 0 {
		return actool.Errorf("缺少任务上下文，拒绝提议策略（策略必须从某个任务里沉淀出来）"), nil
	}
	// State 留空：CreateStrategy 会强制成 quarantined（新建只允许隔离态）。
	// 这里**没有任何**把它变成 active 的路径 —— 晋级是宿主侧的裁决写。
	s := &db.Strategy{
		Name:            args.Name,
		Summary:         args.Summary,
		Activation:      args.Activation,
		Steps:           args.Steps,
		SuccessCriteria: args.SuccessCriteria,
	}
	strategyID, err := store.CreateStrategy(s)
	if err != nil {
		// CreateStrategy 的硬约束在这里同样生效：正文含真实目标地址 ⇒ 直接拒写。
		return actool.Errorf(fmt.Sprintf("提议被拒: %v", err)), nil
	}
	refs := parseStrategyEvidenceRefs(args.Evidence)
	for _, ref := range refs {
		if err := store.LinkStrategyEvidence(strategyID, ref.Type, ref.ID, "created_by", ref.Digest); err != nil {
			// 证据挂不上时策略已经在库里了（隔离态、不进检索、不会生效），
			// 但提议不登记 —— 一条没有出处的策略不该被人当成「有依据的提议」。
			return actool.Errorf(fmt.Sprintf("策略 %d 已建（隔离态），但证据挂载失败、提议未登记: %v", strategyID, err)), nil
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"name":             s.Name,
		"summary":          s.Summary,
		"activation":       s.Activation,
		"steps":            s.Steps,
		"success_criteria": s.SuccessCriteria,
	})
	proposalID, err := store.CreateStrategyProposal(&db.StrategyProposal{
		StrategyID: strategyID,
		Route:      db.StrategyRouteCreateNew,
		Payload:    payload,
		Reason:     strings.TrimSpace(args.Reason),
		CreatedBy:  t.worker,
	})
	if err != nil {
		return actool.Errorf(fmt.Sprintf("策略 %d 已建（隔离态），但提议登记失败: %v", strategyID, err)), nil
	}
	return jsonResult(map[string]any{
		"strategy_id": strategyID,
		"proposal_id": proposalID,
		"state":       db.StrategyQuarantined,
		"proposal_status": db.StrategyProposalPending,
		"note":        "只提议：策略停在隔离态，需 curator 复核并过重放闸门后才可能生效。",
	})
}

// strategyEvidenceRef 是一条证据引用。
type strategyEvidenceRef struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// parseStrategyEvidenceRefs 解析证据引用列表（也接受单个对象）。
//
// 解析失败退化成空列表（fail closed）：坏 JSON 不该让提议被当成「有证据」。
func parseStrategyEvidenceRefs(raw json.RawMessage) []strategyEvidenceRef {
	if len(raw) == 0 {
		return nil
	}
	var list []strategyEvidenceRef
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var one strategyEvidenceRef
	if err := json.Unmarshal(raw, &one); err == nil && one.ID != "" {
		return []strategyEvidenceRef{one}
	}
	return nil
}

// strategySearchKeeps 按关键词收窄【字面路径】命中的候选。
//
// 结构命中的候选【一律保留】：结构路径捕捉的是「同一个形状」，与字面重叠无关；
// 用关键词把它们筛掉等于把结构路径废掉。
//
// 关键词的口径是「name + summary 的子串包含」（大小写不敏感）：这是工具层的二次
// 收窄，不是检索准入判定（准入判定在 db 层，按 bigram 相似度 + 结构匹配）。
func strategySearchKeeps(matches []db.StrategyMatch, query string) []db.StrategyMatch {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return matches
	}
	out := make([]db.StrategyMatch, 0, len(matches))
	for _, m := range matches {
		if m.StructuralMatch {
			out = append(out, m)
			continue
		}
		if strings.Contains(strings.ToLower(m.Name+" "+m.Summary), q) {
			out = append(out, m)
		}
	}
	return out
}

// strategyDB 返回策略库访问句柄（nil = 不可用）。
//
// 任务绑定的 ExplorationStore 就是策略库所在的那个连接（同一个 *db.DB），
// 所以这里不需要额外的注入口。
func (t *ToolSet) strategyDB() *db.DB {
	if t.ts == nil {
		return nil
	}
	return t.ts.DB()
}

// RenderStrategySummaries 把快照渲染成注入 planner 的文本。
//
// ⚠️ **只渲染 name + summary + utility**，绝不渲染 steps —— steps 是命令模板，
// 进了上下文就等于「把可执行产物塞进上下文」（反模式 8），而本阶段没有执行前的
// 拦截关口。
//
// 结构性保证：入参类型 db.StrategySnapshotEntry **没有 steps 字段** ⇒ 这里拿不到
// steps，也就没有「不小心把正文渲染进去」的可能。注入路径的摘要只可能来自这里。
func RenderStrategySummaries(entries []db.StrategySnapshotEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- #%d v%d [utility=%.2f] %s：%s\n",
			e.StrategyID, e.Version, e.Utility, e.Name, e.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}
