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

var auditorSema = make(chan struct{}, 2)

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
const auditorPrompt = `你是渗透测试平台的审计裁决岗（auditor）。平台把一次待裁决的工具调用交给你，你的职责：按任务目标与授权范围判断这次调用该放行（pass）、拒绝（deny）还是升级人工确认（ask）。

原则（按序）：
1. 锁定基线（删除/破坏、平台凭据、平台自身配置）永远到不了你这里——你无需也无法放行它们。
2. 不确定就 ask：宁可让多疑的人看一眼，也不猜。绝不编造目标或意图。
3. deny 必须给可读理由与改道建议（worker 能看懂为什么、该怎么调整）。
4. 高危疑似（凭据外传痕迹、破坏征兆、明显与任务无关的探测）→ anomaly_flag=true 并说明。
5. 若你反复裁决同一形状的调用，用 suggest_category 建议把它沉淀为拦截种类（确定性规则），减少未来问你的次数。

输出契约：恰好一次 sod_auditor_verdict 工具调用。verdict 只能是 pass|deny|ask；reason 用中文一句话；pass 时 target_hint 给出本次调用的目标（域名/IP/路径），供范围审计。`

// auditorVerdictSchema 是裁决的工具调用契约。
func auditorVerdictSchema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        "sod_auditor_verdict",
		Description: "提交一次审计裁决",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"verdict":        map[string]any{"type": "string", "enum": []string{"pass", "deny", "ask"}},
				"reason_code":    map[string]any{"type": "string"},
				"reason":         map[string]any{"type": "string"},
				"category":       map[string]any{"type": "string"},
				"target_hint":    map[string]any{"type": "string"},
				"anomaly_flag":   map[string]any{"type": "boolean"},
				"anomaly_reason": map[string]any{"type": "string"},
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
	// 45s：与 normalizer 同档——快模型在网关抖动时 20s 会被砍（实测），
	// 超时→fallback ask→worker 步骤预算内等不到人→deny，等于 auditor 形同虚设。
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
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
		Verdict         string `json:"verdict"`
		ReasonCode      string `json:"reason_code"`
		Reason          string `json:"reason"`
		Category        string `json:"category"`
		TargetHint      string `json:"target_hint"`
		AnomalyFlag     bool   `json:"anomaly_flag"`
		AnomalyReason   string `json:"anomaly_reason"`
		SuggestCategory *struct {
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
		Verdict:       verdict,
		ReasonCode:    firstNonEmptyStr(raw.ReasonCode, "auditor_"+verdict),
		Reason:        strings.TrimSpace(raw.Reason),
		Category:      strings.TrimSpace(raw.Category),
		TargetHint:    strings.TrimSpace(raw.TargetHint),
		AnomalyFlag:   raw.AnomalyFlag,
		AnomalyReason: strings.TrimSpace(raw.AnomalyReason),
		DecidedBy:     "auditor",
		LatencyMs:     latency,
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
		if prov, _, pok := s.providerForProfile(pol.PrimaryID); pok && prov != nil {
			return prov, true
		}
		log.Printf("[role-policy] auditor lane 的 profile #%d 不可用，回退活动 profile", pol.PrimaryID)
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
