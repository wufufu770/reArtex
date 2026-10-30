// 本文件把「一次运行」变成「若干条带归因结论的证据」。
//
// 移植 SKILLSVOTE 的「子任务级归因 + 证据门控」（design §2）：
//
//	Step 1 切子任务：一个目标 + 一个主要评价信号 + 至多一个策略上下文
//	Step 2 归因：skill_guided / independent / environment
//	Step 3 门控与聚合：失败子任务永不直接改库；同类证据先聚合再改库
//	Step 4 提议：agent 不能直接写库，只能提议
//
// 【本文件只写 usage / evidence / proposal，绝不写 strategies.state】——
// 晋级是裁决写，只能在宿主侧发生（auto-permission-tiers §4.2）。
//
// 【归因的时机】按**意图完成**触发一次，不是按工具步骤：
// 逐工具归因会把一次成功碎成十几个「命中」，于是「一次成功 = 通用能力」
// （反模式 10）。子任务级归因的粒度就是「一个目标 + 一个评价信号」。
//
// 【为什么归因不经过 LLM】：里程碑命中是**确定性**的字符串 / 正则匹配
// （判据写在策略的 success_criteria 里）。让模型说「这条算不算命中」就是
// 粗糙 judge（反模式 4），而 utility 只允许来自客观里程碑（design §4.5）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为四个文件（两个新建 + 两个测试）；
// 切分 / 归因 / 里程碑判定 / 提议路由共用同一条「先聚合、后改库」的链路，
// 拆开会把「失败子任务永不改库」这道闸门切散。
package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 归因三分类。值以 db 包为唯一真相源（server 层只做别名），
// 免得同一套词在两个包里各写一遍、各改一遍。
const (
	// AttributionSkillGuided = 策略在上下文里且它的里程碑命中了 ⇒ 策略有功。
	AttributionSkillGuided = db.StrategyAttributionSkillGuided
	// AttributionIndependent = 策略在上下文里但里程碑没命中 ⇒ 成功是别的路径
	// 挣来的。策略因此只是**候选来源**，不是胜利。
	AttributionIndependent = db.StrategyAttributionIndependent
	// AttributionEnvironment = 没有策略上下文，或上下文里有多条策略 ⇒ 不可归因。
	AttributionEnvironment = db.StrategyAttributionEnvironment
)

// attributionActor 是提议的署名。归因器是宿主侧组件，不是某个 agent。
const attributionActor = "strategy_attributor"

// SubTaskAttribution 是单个子任务的归因结论。
type SubTaskAttribution struct {
	NodeID         int64
	Classification string // skill_guided | independent | environment
	StrategyIDs    []int64
	MilestonesHit  []string
	Reason         string

	// Procedure 是子任务产出的「可复用件」（可空）。
	//
	// 为什么它是可选的：绝大多数子任务只是「用了一条已有策略」，此时路由是
	// edit_existing；只有当子任务总结出了一个【库里还没有】的可复用步骤序列时
	// 才有 create_new —— 而「总结出可复用件」这件事只有工具（record_fact 里
	// 写下的步骤）或人知道，**归因器自己不发明它**（它不做 LLM 判断）。
	Procedure *StrategyProcedure
}

// StrategyProcedure 是一个可复用件的描述（提议 create_new 时的载荷）。
type StrategyProcedure struct {
	Name       string          `json:"name"`
	Summary    string          `json:"summary"`
	Activation json.RawMessage `json:"activation,omitempty"`
	Steps      json.RawMessage `json:"steps,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// AttributeSubTasks 把一次运行的子任务切成三类并聚合证据票。
//
// 子任务的切法：一个 fact / finding 节点 = 一个子任务
//
//	目标       = 节点的 summary（payload.summary）
//	评价信号   = 该意图下命中的里程碑（确定性匹配，见 MilestoneHit）
//	策略上下文 = 该子任务所属意图上记录过的 strategy_usage 行
//
// 只有 skill_guided 才给策略加分（反模式 10）。**失败子任务永不直接改库**，
// 只作为诊断证据（design §2 Step 3）：它只写一行 usage（outcome='miss'）。
//
// 归因按**意图完成**触发一次，不是按工具步骤触发。
func AttributeSubTasks(pg *db.DB, taskID int64) ([]SubTaskAttribution, error) {
	task, err := pg.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("读任务 %d 失败: %w", taskID, err)
	}
	if task == nil {
		return nil, fmt.Errorf("任务 %d 不存在", taskID)
	}
	store := pg.Exploration(task.ExplorationID)

	nodes, err := collectSubTaskNodes(store)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	intents, err := producingIntents(store, nodes)
	if err != nil {
		return nil, err
	}
	usages, err := pg.StrategyUsageForTask(taskID)
	if err != nil {
		return nil, err
	}
	byIntent := map[int64][]db.StrategyUsage{}
	for _, u := range usages {
		byIntent[u.IntentID] = append(byIntent[u.IntentID], u)
	}

	// 按意图分组：一个意图（= 一次 worker 执行）产出的若干 fact / finding
	// 共享同一份策略上下文与同一份里程碑命中 ⇒ 归因结论按意图聚合一次。
	groups := groupByIntent(nodes, intents)
	attrs := make([]SubTaskAttribution, 0, len(nodes))
	votes := make([]db.StrategyVote, 0, len(groups))
	for _, g := range groups {
		ctx := distinctStrategyIDs(byIntent[g.IntentID])
		switch {
		case len(ctx) == 0:
			// 没有策略上下文 ⇒ 成功由环境决定，不可归因。
			for _, n := range g.Nodes {
				attrs = append(attrs, SubTaskAttribution{
					NodeID:         n.ID,
					Classification: AttributionEnvironment,
					Reason:         "子任务没有策略上下文：结果由环境决定，不可归因",
				})
			}
			continue
		case len(ctx) > 1:
			// 「至多一个策略上下文」的前提被破坏 ⇒ 不可归因，且**不给任何策略
			// 记功也不记过**（上下文不唯一是检索侧的问题，不是策略的问题）。
			for _, n := range g.Nodes {
				attrs = append(attrs, SubTaskAttribution{
					NodeID:         n.ID,
					Classification: AttributionEnvironment,
					StrategyIDs:    ctx,
					Reason: fmt.Sprintf("策略上下文不唯一（%d 条）：「至多一个策略上下文」的前提被破坏，不给任何策略加分",
						len(ctx)),
				})
			}
			continue
		}
		strategyID := ctx[0]
		hits, err := strategyMilestoneHits(pg, store, strategyID, g.IntentID)
		if err != nil {
			return nil, err
		}
		guided := make([]*db.Node, 0, len(g.Nodes))
		for _, n := range g.Nodes {
			// 里程碑命中 + 结论被接受 ⇒ 策略有功。
			// 结论被 dismissed（子任务失败 / 结论被推翻）⇒ 即便命中过里程碑也不记功。
			if len(hits) > 0 && n.State != "dismissed" {
				guided = append(guided, n)
			}
		}
		class := AttributionIndependent
		outcome := db.StrategyUsageMiss
		if len(guided) > 0 {
			class = AttributionSkillGuided
			outcome = db.StrategyUsageHit
		}
		// usage 是真相源：失败子任务也写（outcome='miss'），但**只写 usage**。
		if err := pg.RecordStrategyUsage(db.StrategyUsage{
			StrategyID:     strategyID,
			TaskID:         taskID,
			ExplorationID:  task.ExplorationID,
			IntentID:       g.IntentID,
			Outcome:        outcome,
			Classification: class,
			MilestoneHits:  hits,
		}); err != nil {
			return nil, err
		}
		for _, n := range g.Nodes {
			a := SubTaskAttribution{
				NodeID:         n.ID,
				Classification: class,
				StrategyIDs:    ctx,
				MilestonesHit:  hits,
			}
			if class == AttributionSkillGuided {
				a.Reason = "策略在上下文里且里程碑命中：策略有功"
			} else {
				a.Reason = "策略在上下文里但里程碑未命中：成功不归功于该策略"
			}
			attrs = append(attrs, a)
		}
		if len(guided) > 0 {
			votes = append(votes, db.StrategyVote{
				StrategyID:      strategyID,
				TaskID:          taskID,
				ExplorationID:   task.ExplorationID,
				IntentID:        g.IntentID,
				NodeID:          guided[0].ID,
				Classification:  AttributionSkillGuided,
				Outcome:         outcome,
				MilestoneHits:   hits,
				EvidenceRefType: "activity",
				EvidenceRefID:   strconv.FormatInt(g.IntentID, 10),
				ContentDigest:   "sha256:" + strconv.FormatInt(g.IntentID, 10),
			})
		}
	}
	// 同类证据先聚合，再改库：整批投票一次落库，不逐条改。
	if err := pg.ApplyStrategyVotes(votes); err != nil {
		return nil, err
	}
	return attrs, nil
}

// ProposeFromAttributions 把归因结果路由成 create_new / edit_existing / skip。
//
// 路由口径：
//
//	edit_existing —— 库里已有这条策略（或同名策略）⇒ 提议是对它的升版 / 补证据
//	create_new    —— 子任务总结出了一个库里还没有的可复用件
//	skip          —— 没有可复用的信号
//
// ⚠️ 路由只产生**待裁决的提议**（status='pending'），裁决在宿主侧
// （Task 5 curator / Task 6 operator API）。agent 侧**永远写不出 active**。
func ProposeFromAttributions(pg *db.DB, taskID int64, attrs []SubTaskAttribution) ([]int64, error) {
	known, err := pg.ListStrategies("", 1000)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(attrs))
	for _, a := range attrs {
		// 只有 skill_guided 值得提议：independent 的成功不是策略挣来的，
		// environment 根本不可归因。
		if a.Classification != AttributionSkillGuided {
			continue
		}
		route, strategyID, payload := routeAttribution(pg, a, known)
		if route == "" {
			continue
		}
		id, err := pg.CreateStrategyProposal(&db.StrategyProposal{
			StrategyID: strategyID,
			Route:      route,
			Payload:    payload,
			Reason:     proposalReason(a, route),
			CreatedBy:  attributionActor,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// routeAttribution 判定一条归因该路由成哪种提议。
//
// 第二个返回值是策略 id（create_new 时为 0 —— 策略还没建）。
func routeAttribution(pg *db.DB, a SubTaskAttribution, known []*db.Strategy) (string, int64, json.RawMessage) {
	if a.Procedure != nil && a.Procedure.Name != "" {
		// 同名策略已经存在 ⇒ 改它，不要再造一条同名（同名唯一是库级硬约束）。
		for _, s := range known {
			if s.Name == a.Procedure.Name {
				return db.StrategyRouteEditExisting, s.ID, editPayload(s, a)
			}
		}
		// 库里没有同名策略 ⇒ 新 procedure。
		payload, err := json.Marshal(map[string]any{
			"procedure": a.Procedure,
			"task_id":   a.NodeID,
			"rationale": proposalReason(a, db.StrategyRouteCreateNew),
		})
		if err != nil {
			return "", 0, nil
		}
		return db.StrategyRouteCreateNew, 0, payload
	}
	if len(a.StrategyIDs) == 1 {
		s := findStrategy(known, a.StrategyIDs[0])
		if s == nil {
			// 策略已被删 ⇒ 没有可改的对象，也没有可复用的信号。
			return "", 0, nil
		}
		return db.StrategyRouteEditExisting, s.ID, editPayload(s, a)
	}
	return "", 0, nil
}

// editPayload 是「改已有策略」提议的载荷。
func editPayload(s *db.Strategy, a SubTaskAttribution) json.RawMessage {
	payload, err := json.Marshal(map[string]any{
		"strategy_id":    s.ID,
		"version":        s.Version + 1,
		"milestone_hits": a.MilestonesHit,
		"name":           s.Name,
		"summary":        s.Summary,
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return payload
}

// proposalReason 给出提议理由（中文，进库可读）。
func proposalReason(a SubTaskAttribution, route string) string {
	hits := strings.Join(a.MilestonesHit, "、")
	if hits == "" {
		hits = "（无）"
	}
	switch route {
	case db.StrategyRouteCreateNew:
		return fmt.Sprintf("子任务 #%d 总结出一个库里还没有的可复用件，命中里程碑：%s", a.NodeID, hits)
	default:
		return fmt.Sprintf("子任务 #%d 在策略上下文里命中里程碑：%s", a.NodeID, hits)
	}
}

// findStrategy 在已知策略列表里按 id 找一条。
func findStrategy(known []*db.Strategy, id int64) *db.Strategy {
	for _, s := range known {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// MilestoneHit 判定某个子任务命中了策略的哪些里程碑。
//
// 命中判定是【确定性】的：对 activity 行（tool / summary / detail）做字符串或
// 正则匹配。**绝不让 LLM 说「这条算不算命中」** —— 那是粗糙 judge（反模式 4），
// 而 utility 只允许来自客观里程碑（design §4.5）。
//
// success_criteria 的每项形状：
//
//	{"id":"m1","where":"detail","match":"(?i)syntax error","kind":"command"}
//
// where 取 tool / summary / detail（留空 = 三处都看）；kind='command' 时只看
// 带工具名的执行步骤；match 含正则元字符时按正则匹配，否则按字面子串匹配。
func MilestoneHit(criteria []byte, acts []db.Activity) []string {
	items := parseMilestoneCriteria(criteria)
	if len(items) == 0 {
		return nil
	}
	var hits []string
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		if it.matches(acts) {
			hits = append(hits, it.ID)
		}
	}
	return hits
}

// milestoneCriterion 是一条里程碑判据。
type milestoneCriterion struct {
	ID    string `json:"id"`
	Where string `json:"where"`
	Match string `json:"match"`
	Kind  string `json:"kind"`
}

// parseMilestoneCriteria 解析策略的 success_criteria。
//
// 坏 JSON ⇒ 空判据（fail closed）：宁可判定不命中，也不让一份坏 JSON 变成
// 「什么都命中」。
func parseMilestoneCriteria(criteria []byte) []milestoneCriterion {
	if len(criteria) == 0 {
		return nil
	}
	var items []milestoneCriterion
	if err := json.Unmarshal(criteria, &items); err != nil {
		return nil
	}
	return items
}

// matches 判定一条判据是否被这些 activity 命中。
func (c milestoneCriterion) matches(acts []db.Activity) bool {
	if c.Match == "" {
		return false
	}
	re, literal := c.compile()
	for _, a := range acts {
		if c.Kind == "command" && !isCommandActivity(a) {
			continue
		}
		for _, text := range c.texts(a) {
			if text == "" {
				continue
			}
			if re != nil {
				if re.MatchString(text) {
					return true
				}
				continue
			}
			if strings.Contains(text, literal) {
				return true
			}
		}
	}
	return false
}

// compile 编译判据的匹配式。
//
// 口径：match 里带正则元字符 ⇒ 按正则（"(?i)syntax error" 这类显式正则）；
// 不带 ⇒ 按字面子串（避免 "a.b" 把 "axb" 也算命中）。
func (c milestoneCriterion) compile() (*regexp.Regexp, string) {
	if regexp.QuoteMeta(c.Match) != c.Match {
		if re, err := regexp.Compile(c.Match); err == nil {
			return re, ""
		}
	}
	return nil, c.Match
}

// texts 按 where 取出这条 activity 上要比对的文本。
func (c milestoneCriterion) texts(a db.Activity) []string {
	switch c.Where {
	case "tool":
		return []string{a.Tool}
	case "summary":
		return []string{a.Summary}
	case "detail":
		return []string{a.Detail}
	default:
		return []string{a.Tool, a.Summary, a.Detail}
	}
}

// isCommandActivity 判定一条 activity 是不是「命令执行步骤」。
func isCommandActivity(a db.Activity) bool {
	return a.Tool != "" || a.Kind == "tool_use" || a.Kind == "tool_result"
}

// needsActivityDetail 判定里程碑判据是否要看 activity 的 detail 字段。
//
// detail 不在 activity 列表查询里（那是懒加载的大字段）⇒ 只有真的要看时才逐条
// 取，避免每个子任务都打 N 次库。
func needsActivityDetail(criteria []byte) bool {
	for _, c := range parseMilestoneCriteria(criteria) {
		if c.Where == "" || c.Where == "detail" {
			return true
		}
	}
	return false
}

// strategyMilestoneHits 算出某条策略在一个意图上命中了哪些里程碑。
func strategyMilestoneHits(pg *db.DB, store *db.ExplorationStore, strategyID, intentID int64) ([]string, error) {
	st, err := pg.GetStrategy(strategyID)
	if err != nil {
		return nil, fmt.Errorf("读策略 %d 失败: %w", strategyID, err)
	}
	if st == nil {
		return nil, nil
	}
	acts, err := intentActivities(store, intentID, needsActivityDetail(st.SuccessCriteria))
	if err != nil {
		return nil, err
	}
	return MilestoneHit(st.SuccessCriteria, acts), nil
}

// intentActivities 取一个意图的执行步骤（必要时补上 detail）。
func intentActivities(store *db.ExplorationStore, intentID int64, withDetail bool) ([]db.Activity, error) {
	if intentID <= 0 {
		return nil, nil
	}
	acts, _, err := store.ActivityList(&intentID, 0, 300)
	if err != nil {
		return nil, fmt.Errorf("读意图 %d 的执行记录失败: %w", intentID, err)
	}
	if !withDetail {
		return acts, nil
	}
	for i := range acts {
		detail, err := store.ActivityDetail(acts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("读执行记录 %d 的详情失败: %w", acts[i].ID, err)
		}
		acts[i].Detail = detail
	}
	return acts, nil
}

// intentGroup 是一批共享同一意图的子任务。
type intentGroup struct {
	IntentID int64
	Nodes    []*db.Node
}

// groupByIntent 把子任务节点按产出它们的意图分组。
//
// 为什么按意图分组：一个意图（= 一次 worker 执行）产出的若干 fact / finding
// 共享同一份策略上下文与同一份里程碑命中 ⇒ 归因结论按意图聚合一次，
// usage 行也只写一条（(策略, 任务, 意图) 是唯一键）。
func groupByIntent(nodes []*db.Node, intents map[int64]int64) []*intentGroup {
	order := make([]int64, 0, len(nodes))
	groups := map[int64]*intentGroup{}
	for _, n := range nodes {
		intentID := intents[n.ID]
		g := groups[intentID]
		if g == nil {
			g = &intentGroup{IntentID: intentID}
			groups[intentID] = g
			order = append(order, intentID)
		}
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]*intentGroup, 0, len(order))
	for _, id := range order {
		out = append(out, groups[id])
	}
	return out
}

// collectSubTaskNodes 收集一个任务的子任务节点（fact + finding）。
//
// ⚠️ 按 **exploration 归属**取，不按 scope：归因要回答的是「这个任务内部
// 发生了什么」，不是「谁能看见」。范围隔离管的是读面可见性，管不到归因。
func collectSubTaskNodes(store *db.ExplorationStore) ([]*db.Node, error) {
	facts, err := store.ListByKind(db.KindFact, 500)
	if err != nil {
		return nil, fmt.Errorf("列事实节点失败: %w", err)
	}
	findings, err := store.ListByKind(db.KindFinding, 500)
	if err != nil {
		return nil, fmt.Errorf("列漏洞节点失败: %w", err)
	}
	out := make([]*db.Node, 0, len(facts)+len(findings))
	for _, n := range facts {
		// origin fact 是任务起点（不是一次 worker 执行的结果）⇒ 不算子任务。
		if n.State == db.StateOrigin {
			continue
		}
		out = append(out, n)
	}
	out = append(out, findings...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// producingIntents 给出每个子任务节点由哪个意图产出。
//
// 血缘走 exploration_edges（intent --yields--> fact / finding，
// record_fact / report_finding 都写这条边）。产不出意图的节点（没有血缘）
// 不在结果里 ⇒ 它们没有策略上下文 ⇒ 归因为 environment。
func producingIntents(store *db.ExplorationStore, nodes []*db.Node) (map[int64]int64, error) {
	if len(nodes) == 0 {
		return map[int64]int64{}, nil
	}
	ids := make([]int64, 0, len(nodes))
	isSubTask := map[int64]bool{}
	for _, n := range nodes {
		ids = append(ids, n.ID)
		isSubTask[n.ID] = true
	}
	edges, err := store.EdgesTouching(ids)
	if err != nil {
		return nil, fmt.Errorf("读血缘边失败: %w", err)
	}
	srcs := make([]int64, 0, len(edges))
	for _, e := range edges {
		switch e.Rel {
		case db.RelYields, db.RelDerivedFrom, db.RelSpawns:
			srcs = append(srcs, e.From)
		}
	}
	if len(srcs) == 0 {
		return map[int64]int64{}, nil
	}
	srcNodes, err := store.NodesByIDs(srcs)
	if err != nil {
		return nil, fmt.Errorf("读血缘节点失败: %w", err)
	}
	isIntent := map[int64]bool{}
	for _, n := range srcNodes {
		if n.Kind == db.KindIntent {
			isIntent[n.ID] = true
		}
	}
	out := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelYields, db.RelDerivedFrom, db.RelSpawns:
		default:
			continue
		}
		if isSubTask[e.To] && isIntent[e.From] {
			out[e.To] = e.From
		}
	}
	return out, nil
}

// distinctStrategyIDs 取一条策略上下文里的**不同**策略 id。
//
// ⚠️ 「不同」很重要：同一条策略在一次意图里被记了多行（不同轮次）仍然是
// 「一个策略上下文」；只有两条**不同**策略才破坏「至多一个策略上下文」的前提。
func distinctStrategyIDs(usages []db.StrategyUsage) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(usages))
	for _, u := range usages {
		if u.StrategyID <= 0 || seen[u.StrategyID] {
			continue
		}
		seen[u.StrategyID] = true
		out = append(out, u.StrategyID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TaskReplayMilestones 汇总一个任务里命中过的里程碑（冻结语料的输入之一）。
func TaskReplayMilestones(pg *db.DB, taskID int64) ([]string, error) {
	usages, err := pg.StrategyUsageForTask(taskID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, u := range usages {
		for _, h := range u.MilestoneHits {
			if seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TaskReplayOutcomes 汇总一个任务的漏洞结局（冻结语料的输入之一）。
func TaskReplayOutcomes(pg *db.DB, taskID int64) ([]string, error) {
	findings, _, err := pg.ListFindingsPage(db.FindingFilter{TaskID: strconv.FormatInt(taskID, 10)}, 1, 500)
	if err != nil {
		return nil, fmt.Errorf("读任务 %d 的漏洞失败: %w", taskID, err)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, f := range findings {
		if f.Status == "" || seen[f.Status] {
			continue
		}
		seen[f.Status] = true
		out = append(out, f.Status)
	}
	sort.Strings(out)
	return out, nil
}
