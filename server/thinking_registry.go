package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
)

// 模型思考档位能力注册表（方案 v9.2 §2.1，批次 A）。
//
// 没有行业标准的能力发现端点（/models 不返回能力）⇒ 三层解法的第一层：
// 代码内置 注册表（格式×模型名 pattern → 档位面），第二层是 /api/llm/thinking-probe
// 探测验证（LLM 配置页「测试思考档位」按钮，今天对 mimo 的探测法产品化），
// 第三层是前端保留下拉「自定义」手动兜底（注册表过时不阻塞使用）。
//
// 维护纪律：条目来自 2026-11 调研（方案 v9.2 §1 的档位面表）与 2026-11-08 的
// mimo 网关实测；每条注记写清依据。模型不匹配任何 pattern ⇒ known=false，
// 前端回退保守全集（与旧版静态下拉同一组值，行为不回归）。

// thinkingShape 档位形态：effort=档位数组（reasoning_effort / output_config.effort
// 参数）；switch=仅 开/关（thinking.type 参数；mimo/glm/deepseek 实测形状）。
// budget（qwen thinking_budget / anthropic budget_tokens 的连续预算）暂不支持，
// 一并以 switch 呈现并注记，避免 UI 出现填不了的输入。
type thinkingShape string

const (
	shapeEffort thinkingShape = "effort"
	shapeSwitch thinkingShape = "switch"
)

// thinkingCapability 是注册表查询单条结果。
type thinkingCapability struct {
	Shape   thinkingShape `json:"shape"`            // effort|switch
	Levels  []string      `json:"levels"`           // shape=effort 的合法档位；switch 固定 [enabled,disabled]
	Default string        `json:"default,omitempty"` // 该模型默认档位（已知时）
	Note    string        `json:"note,omitempty"`    // 面向用户的注记（实测结论/映射说明）
	Known   bool          `json:"known"`             // false=未收录模型（回退保守全集）
}

// effortCap 是一条 effort 形态的注册表条目。
type effortCap struct {
	prefix  string // 模型名小写前缀匹配（"" = 包含匹配）
	contains string
	levels  []string
	def     string
	note    string
}

// thinkingRegistry 按「先具体后一般」排序，首个命中即返回。
var thinkingRegistry = []effortCap{
	{
		contains: "mimo", levels: []string{"enabled", "disabled"},
		note: "实测（2026-11-08）：仅认 thinking 开关；effort 参数（output_config.effort）不支持——发送后返回空响应。且禁思考会使复杂任务输出空响应（normalizer_malformed 根因），建议保持「开」。",
	},
	{
		prefix: "gpt-5.1", levels: []string{"none", "low", "medium", "high", "xhigh"}, def: "none",
		note: "gpt-5.1 档位面：none/low/medium/high/xhigh（none=完全关思考，默认 none）。",
	},
	{
		prefix: "gpt-5", levels: []string{"minimal", "low", "medium", "high"}, def: "medium",
		note: "gpt-5（5.1 之前）档位面：minimal/low/medium/high。",
	},
	{
		prefix: "o1", levels: []string{"low", "medium", "high"}, def: "medium",
		note: "o 系列推理模型：low/medium/high。",
	},
	{
		prefix: "o3", levels: []string{"low", "medium", "high"}, def: "medium",
		note: "o 系列推理模型：low/medium/high。",
	},
	{
		prefix: "o4", levels: []string{"low", "medium", "high"}, def: "medium",
		note: "o 系列推理模型：low/medium/high。",
	},
	{
		prefix: "claude", levels: []string{"low", "medium", "high"}, def: "medium",
		note: "Claude effort 形状（output_config.effort）：low/medium/high；连续预算 budget_tokens 暂不支持（方案 v9.2 批次 C）。",
	},
	{
		prefix: "gemini", levels: []string{"low", "high"}, def: "high",
		note: "Gemini thinkingLevel 映射：low/high（连续 thinkingBudget 暂不支持）。",
	},
	{
		prefix: "glm", levels: []string{"enabled", "disabled"},
		note: "GLM：thinking.type 仅 开/关。",
	},
	{
		prefix: "deepseek", levels: []string{"enabled", "disabled"},
		note: "DeepSeek：thinking 参数开关（不再靠换模型 id）。",
	},
	{
		prefix: "qwen", levels: []string{"enabled", "disabled"},
		note: "Qwen（百炼）原生是 enable_thinking + thinking_budget 连续预算；当前管道经 thinking.type 映射仅 开/关，预算暂不支持。",
	},
}

// unknownModelCapability 是未收录模型的回退：保守全集（与旧版静态下拉同一组值
// 减去 none 哨兵——none 在 UI 里是「不发送」，恒可用），known=false 让前端挂
// 「未验证」注记并保留自定义输入。
func unknownModelCapability() thinkingCapability {
	return thinkingCapability{
		Shape:  shapeEffort,
		Levels: []string{"low", "medium", "high", "xhigh", "max"},
		Note:   "未收录模型：档位未经注册表验证，请用「测试思考档位」实测，或选自定义输入。",
		Known:  false,
	}
}

// lookupThinkingCapability 按（格式, 模型名）查注册表。模型名匹配不区分大小写；
// 格式仅进注记不改变形状（同一模型名走不同网关格式时，参数映射已由 provider 层处理）。
func lookupThinkingCapability(format, model string) thinkingCapability {
	m := lowerTrim(model)
	if m == "" {
		cap := unknownModelCapability()
		cap.Note = "未填模型名：" + cap.Note
		return cap
	}
	for _, e := range thinkingRegistry {
		if e.prefix != "" && len(m) >= len(e.prefix) && m[:len(e.prefix)] == e.prefix {
			return e.capability()
		}
		if e.contains != "" && strings.Contains(m, e.contains) {
			return e.capability()
		}
	}
	return unknownModelCapability()
}

func (e effortCap) capability() thinkingCapability {
	cap := thinkingCapability{
		Shape:   shapeEffort,
		Levels:  e.levels,
		Default: e.def,
		Note:    e.note,
		Known:   true,
	}
	if e.levels[0] == "enabled" { // switch 形态条目
		cap.Shape = shapeSwitch
	}
	return cap
}

func lowerTrim(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return strings.TrimSpace(string(out))
}

// pgThinkingLevels 是 GET /api/llm/thinking-levels?format=&model= 的 handler：
// LLM 配置页/agent 编辑页据此重填思考档位下拉（批次 A 的「直接知道支持哪些档位」）。
func (s *Server) pgThinkingLevels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, 200, lookupThinkingCapability(q.Get("format"), q.Get("model")))
}

// pgThinkingProbe 是 POST /api/llm/thinking-probe 的 handler（批次 A 第二层：
// 探测验证）。对每个候选档发一次最小连接测试（照 testLLM 的 mirror-production
// 惯例带齐 thinking 参数），以「非空响应」判定该档位实际可用，前端回填并灰显
// 探测不过的档位。恒 200，逐档结果在 body 里。
func (s *Server) pgThinkingProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider         string   `json:"provider"`
		Model            string   `json:"model"`
		BaseURL          string   `json:"base_url"`
		Proxy            string   `json:"proxy"`
		APIKey           string   `json:"api_key"`
		ThinkingType     string   `json:"thinking_type"`
		ReasoningEffort  string   `json:"reasoning_effort"`
		ProfileID        *int64   `json:"profile_id"`
		Streaming        *bool    `json:"streaming"`
		SessionHeaderKey string   `json:"session_header_key"`
		Shape            string   `json:"shape"`   // effort|switch（省略时按注册表）
		Levels           []string `json:"levels"`  // 待探测档位；空=用注册表全集
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	tc := lookupThinkingCapability(req.Provider, req.Model)
	if req.Shape == string(shapeSwitch) {
		tc.Shape = shapeSwitch
	}
	if len(req.Levels) > 0 {
		tc.Levels = req.Levels
	}
	if len(tc.Levels) == 0 {
		writeErr(w, 400, "没有可探测的档位")
		return
	}
	if len(tc.Levels) > 6 {
		tc.Levels = tc.Levels[:6] // 一次探测最多 6 档，防止按钮变成长跑
	}

	type probeResult struct {
		Level     string `json:"level"`
		Ok        bool   `json:"ok"`
		LatencyMs int64  `json:"latency_ms,omitempty"`
		Reply     string `json:"reply,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	results := make([]probeResult, 0, len(tc.Levels))
	for _, level := range tc.Levels {
		cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
		// mirror production：除被探测档外，其余思考参数照表单原样带上——
		// 「测过的组合 = 跑任务时真正会发的组合」。
		cfg.ThinkingType = req.ThinkingType
		cfg.ReasoningEffort = req.ReasoningEffort
		if tc.Shape == shapeSwitch {
			cfg.ThinkingType = level // 探测的是开关档
		} else {
			cfg.ReasoningEffort = level // 探测的是强度档
		}
		if req.Streaming != nil {
			cfg.Stream = *req.Streaming
		}
		cfg.SessionHeaderKey = req.SessionHeaderKey
		if req.ProfileID != nil && (cfg.APIKey == "" || cfg.SessionHeaderKey == "") {
			if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
				if cfg.APIKey == "" {
					cfg.APIKey = p.APIKey
				}
				if cfg.SessionHeaderKey == "" {
					cfg.SessionHeaderKey = p.SessionHeaderKey
				}
			}
		}
		if cfg.APIKey == "" {
			s.cfgMu.Lock()
			cfg.APIKey = s.llmCfg.APIKey
			s.cfgMu.Unlock()
		}
		if cfg.APIKey == "" {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "未提供 API Key", "results": results})
			return
		}
		// 与 testLLM 同款 30s 硬超时兜底改 60s：思考型模型的「回复 OK」也可能
		// 带一段推理，30s 在慢端点偏紧；单档超时不影响其余档位继续探测。
		pctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		lat, reply, err := agent.TestConnection(pctx, cfg)
		cancel()
		res := probeResult{Level: level, LatencyMs: lat.Milliseconds(), Reply: truncateReply(reply)}
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Ok = true
		}
		results = append(results, res)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "shape": tc.Shape, "results": results})
}
