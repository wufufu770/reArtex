package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/artex/notify"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// sodIntentToolKey 是意图规整器唯一的输出通道（tools 表里的那一行）。
//
// 为什么单独记一个常量：规整器不是「随便一个带 schema 的工具」都能当 ——
// 它必须**恰好**是那个让模型调用后产出结构化意图的工具。名字写错 ⇒
// 规整器拿到别的 schema ⇒ 模型调了别的工具 ⇒ 规整不合规 ⇒ 每次调用被拒。
const sodIntentToolKey = "emit_structured_intent"

// sodReviewIntentMaxBytes 是交给 redteam 的结构化意图正文长度上限（字节）。
//
// 为什么截断：needs_review 的材料要进 agent 会话，而会话上下文是共享预算；
// 一份超长结构化意图会把 redteam 的上下文挤掉。截断只影响「人看的细节」，
// 不影响判定依据 —— 载荷摘要、原因码、异常标记都在消息的前半段。
const sodReviewIntentMaxBytes = 1024

// sodChainSlot 持有进程级唯一的 SoD 链。
//
// 为什么必须是【单例】而不是「装一个」：链内部持有一次性执行凭证表
// （TokenStore）。凭证由链铸、由执行器消费，两边必须是**同一张表** —— 链铸的
// 凭证绑 (载荷摘要, task_id, source_agent, tool_name)，执行器用同一组四元组查表。
// 同一进程里装两条链 ⇒ 两条链各自的凭证表互不相通 ⇒ 执行器只在其中一张表里
// 找得到 ⇒ 被批准的动作全部被拒。所以这里是「只装一次」的语义。
var sodChainSlot struct {
	mu    sync.Mutex
	chain *sod.Chain
}

// RegisterSoDChain 安装 SoD 链。**只允许安装一次**（第二次返回错误）。
//
// 为什么第二次必须失败而不是覆盖：见 sodChainSlot 的说明 —— 覆盖式安装会让
// 先装好的链与新链各铸各的凭证，执行器只认其中一张表 ⇒ 已批准的动作被拒。
// 所以第二次安装返回错误，且**不动**已装好的链（调用方拿到的仍是第一条链）。
//
// 顺带在这里把凭证存储发布给执行器（sod.SetExecutorTokens）：链铸凭证、执行器
// 消费凭证，两边必须是同一个 TokenStore。发布必须发生在链**装好之后** ——
// 反过来（先发布后装链）的话，一次失败的安装会把执行器指向一张没人铸凭证的
// 空表 ⇒ 执行器一律拒绝，而宿主以为链已经装好了。
func RegisterSoDChain(deps sod.ChainDeps) error {
	sodChainSlot.mu.Lock()
	defer sodChainSlot.mu.Unlock()
	if sodChainSlot.chain != nil {
		return fmt.Errorf("SoD 链已安装，不允许重复安装")
	}
	// inner 留空：宿主给的 hook runner 由 WrapHookRunner 派生视图时注入。
	sodChainSlot.chain = sod.NewChain(nil, deps)
	if deps.Tokens != nil {
		// 链铸、执行器消费 —— 同一个表。少这一步则每一次被批准的动作都会在
		// 执行器侧查不到凭证而被拒（fail-closed，但链等于瘫了）。
		sod.SetExecutorTokens(deps.Tokens)
	}
	return nil
}

// WrapHookRunner 把已安装的 SoD 链套在宿主给的 hook runner 外层。
//
// 未安装 ⇒ 原样返回（既有行为不变，没启用 SoD 的部署不受任何影响）。
// 安装 ⇒ 返回一个「先调 inner、再跑链」的装饰器。
//
// 顺序（重要，不可调换）：链是【装饰器】—— 它第一件事是问 inner（guard 的
// 短路表 / 拦截规则 / 人工 ask），inner 不拦才跑规整 + 闸门。⇒ 链看到的入参
// 是 inner 可能改写后的**最终**字节，凭证绑定的字节 == 真正会执行的字节
// （TOCTOU 口径：审批时看到的字节必须等于执行时跑的字节）。
//
// 实现用 internal/sod 的 Chain.Wrap：它派生一个**共享同一份 deps**、只换一个
// inner 的视图 —— 于是凭证表、闸门、缓存、审计器全都还是同一个，不存在第二份状态。
func WrapHookRunner(inner harness.HookRunner) harness.HookRunner {
	sodChainSlot.mu.Lock()
	chain := sodChainSlot.chain
	sodChainSlot.mu.Unlock()
	if chain == nil {
		return inner
	}
	return chain.Wrap(inner)
}

// SoDChainInstalled 报告链是否已安装（测试与 /api/health 用）。
func SoDChainInstalled() bool {
	sodChainSlot.mu.Lock()
	defer sodChainSlot.mu.Unlock()
	return sodChainSlot.chain != nil
}

// installSoDChain 组装依赖并注册。**由编排任务在 server 启动时调用一次。**
//
// 失败一律返回中文错误并**不装链**：任何必需件缺失时，宁可「这条路径上没有链」
// （宿主行为保持原样，装配方自己决定要不要因此停服），也不要装一条半成品链
// —— 半成品链的表现是「每次工具调用都被拒」或「链根本没接到受管路径上」，
// 两者都比「没有链」更难排查。
//
// 组装顺序是有讲究的：先取提示词与工具 schema（缺了就一定跑不起来），再确认
// 有可用的规整模型，最后才建链。⇒ 报出来的错总是最贴近根因的那一条。
func (s *Server) installSoDChain() error {
	if s == nil || s.m == nil {
		return fmt.Errorf("SoD 链装配失败：服务器未初始化")
	}
	// ⚠️ 幂等短路：链已经装好 ⇒ 直接成功。
	//
	// 为什么「已安装」不是错误：链是**进程级单例**（凭证表只有一张，铸与消费必须
	// 落在同一张表上），所以第二次装链既没有意义也没有副作用 —— 它只会返回
	// 「已安装，不允许重复安装」这个错误。
	//
	// 为什么这条必须在前面的缺件检查**之前**：同进程里若已有别的 Server 实例装过
	// 链（测试里连续起多个 server 就是这种情况），第二次调用会先去查提示词与
	// schema —— 若那时配置恰好不齐，就会报「缺提示词/缺 schema」，而真实状况是
	// 「链早就装好了，一切正常」。
	//
	// 为什么这条在生产上同样重要：宿主若在同进程里起第二个 Server 实例（重启、
	// 测试、嵌入式使用），不幂等就等于「第二个实例把整个进程打死」—— 而它其实
	// 什么都没做错。
	if SoDChainInstalled() {
		return nil
	}
	pg := s.m.pg
	if pg == nil {
		return fmt.Errorf("SoD 链装配失败：数据库未就绪，无法判定授权范围")
	}
	assets := s.m.assets
	if assets == nil {
		return fmt.Errorf("SoD 链装配失败：资产库未就绪，无法判定授权范围")
	}

	prompt, err := sodNormalizerPrompt(pg)
	if err != nil {
		return err
	}
	schema, err := sodIntentToolSchemaFromDB(pg)
	if err != nil {
		return err
	}
	// 缺 LLM profile 不是「装配缺件」，只影响运行期。
	//
	// 规整器的 provider 是**调用期**解析的（internal/sod.NewNormalizer 只要求
	// provider 函数非 nil），所以缺 profile 的真实后果是：链每次调用都解析不到
	// 模型 ⇒ 按 normalizer_unavailable fail-closed 拒绝 ⇒ **不漏放行**。
	//
	// 为什么不能因此 log.Fatalf（整个 server 起不来）：① 链装不上时连诊断界面
	// 都没有，运维只能看进程崩溃日志；② 它会把不依赖模型的用例一起打挂（例如
	// Phase 0 的读面隔离验收测试 TestTier1ReadOnlyToolsNoScopeLeak —— 那个
	// 测试要起一个 server，却与「有没有配模型」毫无关系）。
	//
	// 真正的缺件是提示词与 schema —— 缺了必然每次调用被拒且无从修复，那两条
	// 仍然硬失败（见上）。
	if err := sodNormalizerProfileReady(pg); err != nil {
		log.Printf("[sod] %v（链仍会装上：受管路径上的工具调用将按 fail-closed 被拒）", err)
	}

	// 规整器：提示词 + schema + 模型 provider 三者齐备才建得出来。
	// profileID 传 0 ⇒ 由 provider 解析器在调用时取活动/默认 profile
	//（与 server/intercept.go 的裁判模型装配同一口径）。
	norm, err := sod.NewNormalizer(s.sodProviderFunc(), 0, prompt, schema)
	if err != nil {
		return fmt.Errorf("SoD 链装配失败：%w", err)
	}

	// 一次性执行凭证表：链铸、执行器消费。RegisterSoDChain 会在链装好之后
	// 把同一个表发布给执行器（sod.SetExecutorTokens）。
	tokens := sod.NewTokenStore()

	// 装新链之前先把执行器的凭证表作废。
	//
	// 为什么必须：执行器的凭证表是**进程级单例**且「首次写入生效」。若进程里已经
	// 装过一条链（老表），现在又装一条新链（新表），SetExecutorTokens 会因为
	// 「不是首次」而**忽略**新表 ⇒ 执行器握着老表、链握着新表 ⇒ 新链铸的凭证
	// 在执行器侧永远查不到 ⇒ 每一个被批准的动作都被拒（fail-closed，不漏放行，
	// 但整条链瘫了，而且日志上看不出原因）。
	//
	// 为什么这里重置是安全的：能走到这一行，说明上面的幂等短路没有命中 ⇒ 链槽位
	// 是空的 ⇒ 进程里当前没有在用的链 ⇒ 老表是上一次装链留下的残骸，作废它
	// 不会影响任何在用的东西。
	sod.ResetExecutorTokens()

	deps := sod.ChainDeps{
		Normalizer:   norm,
		Gate:         sod.NewGate(assets, pg),
		Tokens:       tokens,
		Auditor:      sod.NewAuditor(pg),
		Cache:        sod.NewCache(sodpolicy.NormalizerCacheMaxEntries, norm.PromptDigest()),
		Assets:       assets,
		DB:           pg,
		NotifyReview: s.sodNotifyReview,
		AgentKeyOf:   sod.AgentKeyFromContext,
		// B4 降级档：开关每轮现读（热改即刻生效）；分级器只放 Tier1/2 结构化
		// 写类（Tier3 永不豁免——§4 不变式 1）；降级态变化发一条通知事件。
		DegradeEnabled:  s.sodDegradeEnabled,
		DegradedAllow:   agent.IsTier2WriteTool,
		OnDegradeChange: s.notifySodDegrade,
	}
	// 合并闸（方案 v4.1）：三态开关/情报源/种类表/auditor 裁决岗/人工 ask/
	// cap 计数/影子对照——off 态这些通路全部闲置，行为与旧链一致。
	if s.auditorCache == nil {
		s.auditorCache = newAuditorCache()
	}
	s.ensureAuditorAgent()
	s.wireMergedGateDeps(&deps)
	if err := RegisterSoDChain(deps); err != nil {
		return fmt.Errorf("SoD 链装配失败：%w", err)
	}
	return nil
}

// sodNormalizerPrompt 取规整器提示词正文：DB 优先，回落内置。
//
// 为什么 DB 优先：提示词是页面上可编辑的，管理员改过就该以改后的为准。
// 为什么回落内置：agent 行可能还没播种（新建库、或提示词被清空）——
// 内置正文是编译期随二进制分发的，最后一道兜底。
// 两处都没有 ⇒ 装配失败（没有提示词的规整器必然输出不合规 ⇒ 每次调用被拒）。
func sodNormalizerPrompt(pg *db.DB) (string, error) {
	ag, err := pg.GetAgentByKey(sodpolicy.NormalizerAgentKey)
	if err != nil {
		return "", fmt.Errorf("SoD 链装配失败：读取规整 agent 失败：%w", err)
	}
	if ag != nil {
		body, err := pg.CurrentPrompt(ag.ID)
		if err != nil {
			return "", fmt.Errorf("SoD 链装配失败：读取规整提示词失败：%w", err)
		}
		if strings.TrimSpace(body) != "" {
			return body, nil
		}
	}
	body, ok := agent.BuiltinPromptBody(sodpolicy.NormalizerAgentKey)
	if !ok || strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("SoD 链装配失败：规整器提示词缺失（DB 与内置均无 %q 的提示词）", sodpolicy.NormalizerAgentKey)
	}
	return body, nil
}

// sodIntentToolSchemaFromDB 从 tools 表取 emit_structured_intent 的 schema。
func sodIntentToolSchemaFromDB(pg *db.DB) (llm.ToolSchema, error) {
	tools, err := pg.ListTools()
	if err != nil {
		return llm.ToolSchema{}, fmt.Errorf("SoD 链装配失败：读取工具目录失败：%w", err)
	}
	return sodIntentToolSchema(tools)
}

// sodIntentToolSchema 从工具目录里挑出 emit_structured_intent 那一行，转成
// 规整器要的 llm.ToolSchema。
//
// 为什么必须取到那一行：规整器靠这个 schema 让模型**调用**工具来产出结构化
// 意图；schema 缺失 ⇒ 模型没有可调用的工具 ⇒ 规整必然不合规 ⇒ 每次调用被拒。
// 所以行缺失时直接报错（fail closed），**绝不**拿空 schema 蒙混过关。
//
// 为什么拆成纯函数：工具目录是 DB 里的行，但「挑哪一行、缺了怎么办」这条
// 口径必须能脱离数据库被验证 —— 否则这条 fail-closed 只能靠真库跑得到。
func sodIntentToolSchema(tools []*db.Tool) (llm.ToolSchema, error) {
	for _, t := range tools {
		if t == nil || t.Key != sodIntentToolKey {
			continue
		}
		input := map[string]any{}
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &input); err != nil {
				return llm.ToolSchema{}, fmt.Errorf("SoD 链装配失败：工具 %q 的 schema 不是合法 JSON：%w", sodIntentToolKey, err)
			}
		}
		if len(input) == 0 {
			return llm.ToolSchema{}, fmt.Errorf("SoD 链装配失败：工具 %q 的 schema 为空", sodIntentToolKey)
		}
		return llm.ToolSchema{Name: t.Key, Description: t.Description, InputSchema: input}, nil
	}
	return llm.ToolSchema{}, fmt.Errorf("SoD 链装配失败：工具目录里没有 %q，规整器无法产出结构化意图", sodIntentToolKey)
}

// sodNormalizerProfileReady 检查有没有可用的规整模型。
//
// 为什么在装配期就检查：没有模型 ⇒ 规整器每次都不可用 ⇒ 链每次都按
// normalizer_unavailable 拒绝 ⇒ 整条工具链瘫掉。让人在启动时就看到
// 「没配模型」，比让人在第一次工具调用时看到一堆莫名其妙的拒绝要早得多。
//
// 这里只查「有没有活动/默认 profile」；profile 的密钥等细节由调用期的
// provider 解析器兜底（解析不到 ⇒ 规整器不可用 ⇒ 链按失败关闭拒绝）。
func sodNormalizerProfileReady(pg *db.DB) error {
	p, err := pg.ActiveProfile()
	if err != nil {
		return fmt.Errorf("SoD 链装配失败：读取活动 LLM profile 失败：%w", err)
	}
	if p == nil || p.ID <= 0 {
		// DB 里没有活动 profile ⇒ 还有 env 兜底这条（与 engine 同一口径：
		// server.go 的 loadLLMConfig → agent.FromEnv）。有 env 配置时规整器照样能用，
		// 此时报「未配置」就是误报 ⇒ 而误报会让运维以为链没装好。
		if _, ok := agent.FromEnv(); ok {
			return nil
		}
		return fmt.Errorf("未配置可用的规整模型（活动/默认 LLM profile，也没有 env 兜底）")
	}
	return nil
}

// sodProviderFunc 解析规整器的模型 provider。
//
// 形态照抄 server/intercept.go 的 Reviewer 装配：profileID==0 ⇒ 用活动/默认
// profile；解析不到 ⇒ 返回 ok=false ⇒ 规整器不可用 ⇒ 链按失败关闭拒绝。
// 为什么不在这里缓存 provider：provider 由 Server 侧的 provider 缓存统一管
// （providerForProfile），这里只做「profile id → provider」的解析。
//
// 独立模型档（方案 v3 缺陷 B3）：role_policy 配了 `normalizer` lane 时优先用
// 它的 primary（backup 不在 provider 解析层做——lane 游标是 taskLLM 运行时的
// 机制，规整器直接退回活动 profile 即可，fail-closed 语义不变）。
func (s *Server) sodProviderFunc() sod.ProviderFunc {
	return func(profileID int64) (llm.Provider, bool) {
		if profileID == 0 {
			if pol, ok := s.roleLanePolicy("normalizer"); ok {
				// 同 auditor：lane 的 primary_model 必须参与解析（E1 变体），
				// 否则配置的快模型被忽略、落回 profile 默认的重模型。
				if prov, _, pok := s.providerForProfileModel(pol.PrimaryID, pol.PrimaryModel); pok && prov != nil {
					return prov, true
				}
				log.Printf("[role-policy] normalizer lane 的 profile #%d(%s) 不可用，回退活动 profile", pol.PrimaryID, pol.PrimaryModel)
			}
			if p, err := s.m.pg.ActiveProfile(); err == nil && p != nil {
				profileID = p.ID
			}
		}
		if profileID == 0 {
			// DB 里没有活动 profile ⇒ 退到 engine 已经装好的全局 provider
			// （它可能是从 env 兜底来的，见 server.go 的 loadLLMConfig →
			// agent.FromEnv 那条）。
			//
			// 为什么必须同口径：engine 能用模型、链却报「未配置可用的规整模型」
			// ⇒ 链对**每一次**工具调用都 fail-closed 拒绝 ⇒ 整条链瘫掉，而排查时
			// 看到的是「链没装好」这种误导性现象（实际是链与 engine 用了两套模型
			// 解析口径）。
			//
			// 为什么复用 s.llmProv 而不是另造一个 provider：另造 ⇒ 两份 provider
			// 各带各的限流器 ⇒ 实际请求速率是配置的两倍。
			s.cfgMu.Lock()
			prov := s.llmProv
			s.cfgMu.Unlock()
			return prov, prov != nil
		}
		prov, _, ok := s.providerForProfile(profileID)
		if !ok || prov == nil {
			return nil, false
		}
		return prov, true
	}
}

// settingSodDegradeEnabled 是 B4 降级档的 settings 键（默认 false=fail-closed）。
const settingSodDegradeEnabled = "sod.degrade_enabled"

// sodDegradeEnabled 读降级档开关（链每轮现读；热改即刻生效）。
func (s *Server) sodDegradeEnabled() bool {
	if s.m == nil || s.m.pg == nil {
		return false
	}
	return s.m.pg.GetBool(settingSodDegradeEnabled, false)
}

// isSodDegradableTool 是注入链的降级分级器（B4）：只有 Tier2 结构化写类可以
// 在降级期确定性放行。单一真相源在 agent.IsTier2WriteTool（tier2WriteKinds），
// 这里包一层便于测试与将来加白。
func isSodDegradableTool(toolName string) bool {
	return agent.IsTier2WriteTool(toolName)
}

// notifySodDegrade 把降级态变化写成一条通知事件（进/出各一次）——降级是安全
// 语义变化，必须让人看见，不能只落 server 日志。
func (s *Server) notifySodDegrade(active bool) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	kind := "sod_degrade_entered"
	summary := "SoD 规整器连续不可用，已进入降级档（Tier1/2 确定性放行、Tier3 照拒）。请检查规整模型可用性；恢复后自动退出降级。"
	if !active {
		kind = "sod_degrade_exited"
		summary = "SoD 规整器已恢复，退出降级档（回到 fail-closed）。"
	}
	evt := notify.Snapshot{Kind: kind, Name: "SoD 降级档", Severity: "high", Summary: summary}
	if _, err := s.m.pg.AddNotificationEvent(s.ctx, kind, 0, evt); err != nil {
		log.Printf("[sod] 降级态通知写入失败: %v", err)
	}
}

// sodNotifyReview 把 needs_review 的复核材料交给 redteam。
//
// 为什么要交给**对抗者**：needs_review 交回给提出者复核等于自证（见
// sodpolicy.RedteamAgentKey 的说明）。入队由审计器完成（sod_adversarial_review_pending），
// 这里只负责「即时触发」那条腿 —— 让 redteam 立刻去证伪，而不是等轮询。
//
// best-effort：拉不起 redteam 不能影响链的裁决 —— 裁决早已发生、材料也已入队，
// redteam 迟一点到照样能取到。
func (s *Server) sodNotifyReview(ctx context.Context, item *db.AdversarialReviewItem) {
	if s == nil || s.m == nil || s.ctx == nil || item == nil {
		return
	}
	taskID := int64(0)
	if item.TaskID != nil {
		taskID = *item.TaskID
	}
	msg := fmt.Sprintf(
		"SoD 链裁决 needs_review：工具=%s，来源 agent=%s，原因码=%s，异常=%v，载荷摘要=%s。结构化意图=%s。请在任务授权范围内对该意图证伪。",
		item.ToolName,
		item.SourceAgent,
		item.ReasonCode,
		item.AnomalyFlag,
		item.PayloadDigest,
		truncateSoDReviewIntent(item.StructuredIntent),
	)
	s.StartTriggeredRun(sodpolicy.RedteamAgentKey, "SoD 复核 · "+item.ToolName, msg, taskID, false, "", "")
}

// truncateSoDReviewIntent 截断交给 redteam 的结构化意图正文。
func truncateSoDReviewIntent(intent json.RawMessage) string {
	if len(intent) == 0 {
		return "{}"
	}
	if len(intent) <= sodReviewIntentMaxBytes {
		return string(intent)
	}
	return string(intent[:sodReviewIntentMaxBytes]) + "…(截断)"
}
