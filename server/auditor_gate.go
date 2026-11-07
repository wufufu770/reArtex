package server

// ---------- auditor 合并闸裁决岗（方案 v4.1 §3）----------
//
// internal/sod 的合并闸管线把「模糊动作」交到这里；本文件负责把请求变成一次
// 快模型裁决：
//   - 并发闸 cap 2（复用 normalizer 的 sema 形态；排队在超时闸之外）；
//   - 同 (tool, payload_digest, scope_digest) 裁决缓存——同字节重试零成本；
//   - 模型走 role_policy 的 `auditor` lane（快模型），未配置回活动 profile；
//   - 输出是严格 JSON 工具调用（sod_auditor_verdict），malformed 直接按错误
//     返回（由种类 fallback 兜底，绝不猜）；
//   - suggest_category 落建议队列（best-effort），采纳后沉淀为种类行。
//
// 与 normalizer 的关系：规整器退役（enforce 态不再调用），本岗是链内唯一的
// LLM 裁决阶段。裁决可用性故障不再等于硬墙——链侧按种类 fallback（默认转
// 人工 ask）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/norma/llm"
)

// ---------- 并发闸（形态照抄 normalizerSema）----------

// cap 6（v6 B1 提量）：快模型批量裁决吞吐 ≈ 20+ 次/分钟，覆盖渗透峰值。
var auditorSema = make(chan struct{}, 6)

// ---------- 裁决缓存 ----------

type auditorCacheEntry struct {
	verdict sod.AuditorVerdict
	at      time.Time
}

type auditorCache struct {
	mu      sync.Mutex
	entries map[string]auditorCacheEntry
}

const auditorCacheMax = 4096

func newAuditorCache() *auditorCache {
	return &auditorCache{entries: map[string]auditorCacheEntry{}}
}

func (c *auditorCache) key(req sod.AuditorRequest) string {
	return req.ToolName + "\x00" + req.PayloadDigest + "\x00" + req.ScopeDigest
}

func (c *auditorCache) get(k string) (sod.AuditorVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return sod.AuditorVerdict{}, false
	}
	// 范围一变（scope_digest 进键）自然失效；这里只做 TTL 兜底。
	if time.Since(e.at) > 30*time.Minute {
		delete(c.entries, k)
		return sod.AuditorVerdict{}, false
	}
	e.verdict.DecidedBy = "cache"
	return e.verdict, true
}

func (c *auditorCache) put(k string, v sod.AuditorVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= auditorCacheMax {
		// 简单老化：随机删一个（map 遍历序）；缓存是纯优化，语义无损。
		for kk := range c.entries {
			delete(c.entries, kk)
			break
		}
	}
	c.entries[k] = auditorCacheEntry{verdict: v, at: time.Now()}
}

// ---------- 裁决岗 ----------

// auditorPrompt 是裁决岗的系统提示词（内置兜底；DB 可覆盖，见 auditorPromptOf）。
const auditorPrompt = `你是渗透测试平台的审计裁决岗（auditor）。平台把一次待裁决的工具调用交给你，你的职责：判断这次调用是否【有利于漏洞挖掘且在授权范围内】，据此给出裁决。

裁决哲学（v6，用户拍板）：
- **渗透测试的绝大部分动作都该放行**：探测、扫描、枚举、指纹、目录遍历（低速率）、爬取、情报查询、漏洞探测 payload——只要目标是授权范围内或公开情报源，一律 pass。
- **只有三类必须 ask（人工确认）**：① 对目标系统的写入（上传/创建资源/改配置）；② 爆破类（登录爆破/高速率暴破）；③ 授权边界变更（root_domain/公司级扩权）。删除/破坏类到不了你这里（锁定基线硬拦）。
- **deny 只用于**：明显与任务无关的乱窜（探测平台自身、无关第三方）、以及有凭据外传/数据外泄痕迹的调用。
- exploit_relevance 字段：判断该调用对漏洞挖掘的相关度（high=直接探测/验证漏洞面，medium=信息收集铺垫，none=与挖掘无关）——none 且不在情报源语义里的调用才考虑 deny。

原则（按序）：
1. 锁定基线（删除/破坏、平台凭据、平台自身配置）永远到不了你这里。
2. 不确定就 ask，但「不确定」的门槛是三类人工动作之外的真实疑点——不要把正常的渗透探测当成疑点。
3. deny 必须给可读理由与改道建议。
4. 高危疑似（凭据外传、破坏征兆、平台内省）→ anomaly_flag=true 并说明。
5. 反复裁决同一形状时用 suggest_category 沉淀为拦截种类。

输出契约：恰好一次 sod_auditor_verdict 工具调用。verdict 只能是 pass|deny|ask；reason 用中文一句话；pass 时 target_hint 给出目标；exploit_relevance 填 high|medium|none。`

// auditorVerdictSchema 是裁决的工具调用契约。
func auditorVerdictSchema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        "sod_auditor_verdict",
		Description: "提交一次审计裁决",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"verdict":           map[string]any{"type": "string", "enum": []string{"pass", "deny", "ask"}},
				"reason_code":       map[string]any{"type": "string"},
				"reason":            map[string]any{"type": "string"},
				"category":          map[string]any{"type": "string"},
				"target_hint":       map[string]any{"type": "string"},
				"exploit_relevance": map[string]any{"type": "string", "enum": []string{"high", "medium", "none"}},
				"anomaly_flag":      map[string]any{"type": "boolean"},
				"anomaly_reason":    map[string]any{"type": "string"},
				"suggest_category": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    map[string]any{"type": "string"},
						"pattern": map[string]any{"type": "string"},
						"action":  map[string]any{"type": "string", "enum": []string{"hard_deny", "ask", "auditor", "pass"}},
						"reason":  map[string]any{"type": "string"},
					},
				},
			},
			"required": []string{"verdict", "reason"},
		},
	}
}

// auditorCategoriesSummary 把种类表压成提示词摘要行（v4.1 §10-F4e：控制 prompt 体积）。
func (s *Server) auditorCategoriesSummary() string {
	cats, _ := s.sodCategoriesSnapshot()
	if len(cats) == 0 {
		return "（种类表为空——全部调用由你裁量）"
	}
	var b strings.Builder
	for _, c := range cats {
		if !c.Enabled {
			continue
		}
		marker := ""
		if c.Locked {
			marker = "🔒"
		}
		fmt.Fprintf(&b, "%s%s：动作=%s", marker, c.Name, c.Action)
		if c.CapPerTask > 0 {
			fmt.Fprintf(&b, "（每任务上限 %d）", c.CapPerTask)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sodAuditorAdjudicator 是装配进 ChainDeps.Adjudicator 的裁决入口。
func (s *Server) sodAuditorAdjudicator(ctx context.Context, req sod.AuditorRequest) (sod.AuditorVerdict, error) {
	if s.auditorCache == nil {
		s.auditorCache = newAuditorCache()
	}
	k := s.auditorCache.key(req)
	if v, ok := s.auditorCache.get(k); ok {
		return v, nil
	}

	started := time.Now()
	select {
	case auditorSema <- struct{}{}:
		defer func() { <-auditorSema }()
	case <-ctx.Done():
		return sod.AuditorVerdict{}, fmt.Errorf("等待 auditor 并发槽时调用方取消: %w", ctx.Err())
	}
	// 30s（v6 B2 提速）：glm-5.3-flash 实测中位 ~7s；30s 兜底足够，超时更快
	// 转下一通道（种类 fallback→ask），不让 worker 在单次裁决上吊死。
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 重入标记：auditor 自己的模型调用不得再进链。
	cctx = sod.WithReentrancy(cctx)

	prov, ok := s.auditorProvider()
	if !ok || prov == nil {
		return sod.AuditorVerdict{}, errors.New("auditor 模型不可用")
	}

	// 任务上下文（goal/类别）——查不到就从简。
	goal := ""
	if t, ok := s.m.Task(fmt.Sprintf("%d", req.TaskID)); ok && t != nil {
		goal = trunc(t.Goal, 200)
	}
	userPayload, _ := json.Marshal(map[string]any{
		"tool":           req.ToolName,
		"payload":        req.PayloadPreview,
		"reason":         req.Reason,
		"task_id":        req.TaskID,
		"task_goal":      goal,
		"category_table": s.auditorCategoriesSummary(),
	})

	temp := 0.0
	llmReq := llm.CompletionRequest{
		System:      []string{s.auditorPromptText()},
		Messages:    []llm.Message{llm.UserText(string(userPayload))},
		Tools:       []llm.ToolSchema{auditorVerdictSchema()},
		MaxTokens:   1024,
		Temperature: &temp,
		Thinking:    "disabled",
	}
	acc := llm.NewAccumulator()
	var streamErr error
	for ev, err := range prov.Stream(cctx, llmReq) {
		if err != nil {
			streamErr = err
			break
		}
		acc.Add(ev)
	}
	latency := int(time.Since(started) / time.Millisecond)
	if streamErr == nil {
		streamErr = cctx.Err()
	}
	if streamErr != nil {
		log.Printf("[auditor] 模型调用失败: %v", streamErr)
		s.recordAuditorFailure(req, streamErr, latency)
		return sod.AuditorVerdict{}, fmt.Errorf("auditor 模型调用失败: %w", streamErr)
	}

	uses := acc.Message().ToolUses()
	if len(uses) != 1 || uses[0].Name != "sod_auditor_verdict" {
		return sod.AuditorVerdict{}, fmt.Errorf("auditor 输出不合规（tool_uses=%d）", len(uses))
	}
	var raw struct {
		Verdict          string `json:"verdict"`
		ReasonCode       string `json:"reason_code"`
		Reason           string `json:"reason"`
		ExploitRelevance string `json:"exploit_relevance"`
		Category         string `json:"category"`
		TargetHint       string `json:"target_hint"`
		AnomalyFlag      bool   `json:"anomaly_flag"`
		AnomalyReason    string `json:"anomaly_reason"`
		SuggestCategory  *struct {
			Name    string `json:"name"`
			Pattern string `json:"pattern"`
			Action  string `json:"action"`
			Reason  string `json:"reason"`
		} `json:"suggest_category"`
	}
	if err := json.Unmarshal(uses[0].Input, &raw); err != nil {
		return sod.AuditorVerdict{}, fmt.Errorf("auditor JSON 解析失败: %w", err)
	}
	verdict := strings.ToLower(strings.TrimSpace(raw.Verdict))
	if verdict != "pass" && verdict != "deny" && verdict != "ask" {
		return sod.AuditorVerdict{}, fmt.Errorf("auditor verdict 非法: %q", raw.Verdict)
	}
	out := sod.AuditorVerdict{
		Verdict:          verdict,
		ExploitRelevance: strings.TrimSpace(raw.ExploitRelevance),
		ReasonCode:       firstNonEmptyStr(raw.ReasonCode, "auditor_"+verdict),
		Reason:           strings.TrimSpace(raw.Reason),
		Category:         strings.TrimSpace(raw.Category),
		TargetHint:       strings.TrimSpace(raw.TargetHint),
		AnomalyFlag:      raw.AnomalyFlag,
		AnomalyReason:    strings.TrimSpace(raw.AnomalyReason),
		DecidedBy:        "auditor",
		LatencyMs:        latency,
	}
	s.recordAuditorLLM(req, out, latency)
	if out.Reason == "" {
		out.Reason = "（auditor 未给理由）"
	}
	s.auditorCache.put(k, out)

	// 建议队列（best-effort；v4.1 §3 观察建议循环）。
	if raw.SuggestCategory != nil && raw.SuggestCategory.Name != "" {
		sc := raw.SuggestCategory
		action := sc.Action
		if action != sodpolicy.CategoryActionHardDeny && action != sodpolicy.CategoryActionAsk &&
			action != sodpolicy.CategoryActionAuditor && action != sodpolicy.CategoryActionPass {
			action = sodpolicy.CategoryActionPass
		}
		evidence := []map[string]any{{
			"task_id": req.TaskID, "tool": req.ToolName,
			"payload_digest": req.PayloadDigest, "preview": req.PayloadPreview,
		}}
		if err := s.m.pg.CreateSodCategorySuggestion(sc.Name, "custom", sc.Pattern, action, sc.Reason, evidence); err != nil {
			log.Printf("[auditor] 建议落库失败（忽略）: %v", err)
		}
	}
	return out, nil
}

// recordAuditorFailure 把失败的裁决调用也记进 llm_records（不然黑盒）。
func (s *Server) recordAuditorFailure(req sod.AuditorRequest, cause error, latencyMs int) {
	body, _ := json.Marshal(map[string]any{
		"tool": req.ToolName, "reason": req.Reason, "error": cause.Error(),
	})
	if err := s.m.pg.InsertLLMRecord(&db.LLMRecord{
		Worker: "auditor", Model: "auditor-lane", Status: "error",
		Error: trunc(cause.Error(), 200), LatencyMs: latencyMs,
		RawResponse: string(body),
	}); err != nil {
		log.Printf("[auditor] llm_records 落库失败（忽略）: %v", err)
	}
}

// recordAuditorLLM 把裁决调用记进 llm_records（可观测性：v4 §5-1 的教训）。
func (s *Server) recordAuditorLLM(req sod.AuditorRequest, v sod.AuditorVerdict, latencyMs int) {
	// llm_records 的口径是模型通道审计；auditor 通道记 worker=auditor。
	// 失败不吞——但也不影响裁决。
	body, _ := json.Marshal(map[string]any{
		"tool": req.ToolName, "verdict": v.Verdict, "reason": v.Reason,
		"reason_code": v.ReasonCode, "anomaly": v.AnomalyFlag,
	})
	_, err := "", error(nil)
	err = s.m.pg.InsertLLMRecord(&db.LLMRecord{
		Worker: "auditor", Model: "auditor-lane", Status: "ok",
		LatencyMs: latencyMs, RawResponse: string(body),
	})
	if err != nil {
		log.Printf("[auditor] llm_records 落库失败（忽略）: %v", err)
	}
}

// auditorPromptText 取裁决提示词：DB 提示词目录优先（CurrentPrompt，与
// 规整器 sodNormalizerPrompt 同机制），回落内置。
func (s *Server) auditorPromptText() string {
	if ag, err := s.m.pg.GetAgentByKey(sodpolicy.AuditorAgentKey); err == nil && ag != nil {
		if body, err := s.m.pg.CurrentPrompt(ag.ID); err == nil && strings.TrimSpace(body) != "" {
			return body
		}
	}
	return auditorPrompt
}

// auditorProvider 解析 auditor lane 的 provider（形态照抄 sodProviderFunc：
// lane 优先 → 活动 profile → engine 全局 provider；复用同一 provider 缓存，
// 不另造限流器）。
func (s *Server) auditorProvider() (llm.Provider, bool) {
	if pol, ok := s.roleLanePolicy("auditor"); ok {
		// 必须带 lane 的 primary_model 解析（E1 变体）：不带模型的 providerForProfile
		// 会落 profile 默认模型——实测重推理模型不吐 tool_use，auditor 裁决全部
		// 解析失败（tool_uses=0）→ 每次都转人工。
		if prov, _, pok := s.providerForProfileModel(pol.PrimaryID, pol.PrimaryModel); pok && prov != nil {
			return prov, true
		}
		log.Printf("[role-policy] auditor lane 的 profile #%d(%s) 不可用，回退活动 profile", pol.PrimaryID, pol.PrimaryModel)
	}
	if p, err := s.m.pg.ActiveProfile(); err == nil && p != nil {
		if prov, _, ok := s.providerForProfile(p.ID); ok && prov != nil {
			return prov, true
		}
	}
	s.cfgMu.Lock()
	prov := s.llmProv
	s.cfgMu.Unlock()
	return prov, prov != nil
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
