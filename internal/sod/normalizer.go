package sod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/norma/llm"
)

// 规整器的两种失败语义（对应两个原因码）。
//
// 为什么用哨兵错误而不是一个布尔返回：调用方（链）需要把「模型不可用」与
// 「模型输出不合规」区分开 —— 前者是 normalizer_unavailable，后者是
// normalizer_malformed。两者都是**拒绝**，但原因码不同：前者说明「模型侧没
// 给出结果」，后者说明「模型给了结果但结果不能用」。事后追责时这两种情况的
// 处置完全不同（前者查模型可用性，后者查提示词 / 工具 schema）。
var (
	// ErrNormalizerUnavailable 规整器不可用：超时 / provider 报错 / 无可用 profile。
	ErrNormalizerUnavailable = errors.New("意图规整器不可用")
	// ErrNormalizerMalformed 规整器输出不合规：没调用工具 / 调错工具 / 入参不是
	// 合法 JSON / 缺字段 / 动作类型无法解析。
	ErrNormalizerMalformed = errors.New("意图规整输出不合规")
)

// 动作类型（代码常量）。
//
// 为什么是代码常量：规整器产出的 action_type 决定「按哪种语法规则拆解动作」，
// 而拆解规则只有一份。若允许模型自造动作类型，闸门就可能收到一份它不认识的
// 意图结构 —— 而闸门**不校验**意图结构的合法性（它只读 target_hint 与
// anomaly_flag），于是「不认识的意图」会被静默当成正常意图放行。
const (
	// actionTypeCommandExecution shell 命令执行。
	actionTypeCommandExecution = "command_execution"
	// actionTypeHTTPRequest HTTP 请求。
	actionTypeHTTPRequest = "http_request"
	// actionTypeToolInvocation 工具调用。
	actionTypeToolInvocation = "tool_invocation"
	// actionTypeUnparseable 原始动作无法解析。
	//
	// ⚠️ 它**不在** knownActionTypes 里：见 Normalize 里对它的处理 —— 它是
	// 一条明确的失败信号（normalizer_malformed），不是一个可用的动作类型。
	actionTypeUnparseable = "unparseable"
)

// knownActionTypes 是规整器允许产出的动作类型全集。
var knownActionTypes = map[string]bool{
	actionTypeCommandExecution: true,
	actionTypeHTTPRequest:     true,
	actionTypeToolInvocation:  true,
}

// ProviderFunc 解析一个 LLM profile → provider。返回 ok=false 表示该 profile 不可用。
//
// 形态照抄 server/intercept.go:36-62 的 Reviewer 装配：
// profileID==0 ⇒ 用活动/默认 profile；解析不到 ⇒ 规整器不可用 ⇒ fail-closed。
type ProviderFunc func(profileID int64) (llm.Provider, bool)

// Normalizer 是意图规整器：把原始动作转成结构化意图。
//
// ⚠️ 它【不判断】动作是否合法、是否在 scope 内、是否该执行 ——
// 那是 Gate 的职责（agent/prompts/intent-normalizer.md:10-21）。
//
// ⚠️ 它的产出（NormalizerResult）是**模型产出的数据**，不可信；闸门只把它当
// 结构化线索。本包的铁律「模型只提议，确定性层裁决」就落在这里。
type Normalizer struct {
	provider     ProviderFunc
	profileID    int64
	prompt       string
	promptDigest string
	schema       llm.ToolSchema
	timeout      time.Duration
	maxTokens    int
}

// NewNormalizer 构造规整器。
//
// schema 来自 tools 表的 emit_structured_intent 行；prompt 来自
// intent_normalizer 的 agent 提示词（DB 优先，回落内置）。
// 两者任一取不到 ⇒ 返回 error ⇒ 调用方按 normalizer_unavailable 拒绝。
//
// ⚠️ 为什么提示词与 schema 都是【注入】而不是现取：internal/sod 是叶子包，
// 而提示词在 agent 包（内置正文）、schema 在 db 包（tools 表），两者都会把
// internal/sod 拖进依赖环 ⇒ 由宿主（server/sod_chain.go）装配时取好再传进来。
func NewNormalizer(p ProviderFunc, profileID int64, prompt string, schema llm.ToolSchema) (*Normalizer, error) {
	if p == nil {
		return nil, errors.New("意图规整器未装配模型 provider")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("意图规整器缺少提示词")
	}
	if schema.Name == "" {
		return nil, errors.New("意图规整器缺少 emit_structured_intent 的 schema")
	}
	sum := sha256.Sum256([]byte(prompt))
	return &Normalizer{
		provider:     p,
		profileID:    profileID,
		prompt:       prompt,
		promptDigest: hex.EncodeToString(sum[:]),
		schema:       schema,
		timeout:      time.Duration(sodpolicy.NormalizerTimeoutSeconds) * time.Second,
		maxTokens:    sodpolicy.NormalizerMaxTokens,
	}, nil
}

// PromptDigest 返回提示词摘要（缓存键的一个分量）。
func (n *Normalizer) PromptDigest() string { return n.promptDigest }

// NormalizeInput 是规整器的输入。
type NormalizeInput struct {
	ToolName       string
	RawPayload     []byte
	SourceAgent    string
	TaskID         int64
	ConversationID int64
}

// Normalize 把一次工具调用转成结构化意图。
//
// ⚠️ **每一次失败都返回 error** —— 调用方（Chain）必须按
// normalizer_unavailable / normalizer_malformed 拒绝，**绝不放行**。
//
// 调用形态照抄既有先例 intercept.Interceptor.Judge（intercept/intercept.go:447）
// + server/intercept.go 的 reviewCompletion/streamCollectText：同步一次
// llm.Provider.Stream + llm.CompletionRequest。唯一的差别是本函数额外带
// Tools —— 让模型**必须调用** emit_structured_intent 产出结构化意图，于是
// 输出是结构化的，不需要从散文里抠 JSON。
func (n *Normalizer) Normalize(ctx context.Context, in NormalizeInput) (NormalizerResult, error) {
	prov, ok := n.provider(n.profileID)
	if !ok || prov == nil {
		return NormalizerResult{}, fmt.Errorf("%w: 未找到可用的模型 profile %d", ErrNormalizerUnavailable, n.profileID)
	}
	cctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	// 重入标记：规整器自己的模型调用不得再进链（否则无限递归）。
	// 执行器装饰器看到标记 ⇒ 跳过凭证校验（直连）。
	cctx = withReentrancy(cctx)

	user, err := json.Marshal(map[string]any{
		"raw_action_kind": inferActionKind(in.ToolName),
		"raw_payload":     rawPayloadForPrompt(in.RawPayload),
		"source_agent":    in.SourceAgent,
		"task_id":         in.TaskID,
		"conversation_id": in.ConversationID,
	})
	if err != nil {
		return NormalizerResult{}, fmt.Errorf("%w: 构造规整输入失败: %v", ErrNormalizerUnavailable, err)
	}

	temp := 0.0
	req := llm.CompletionRequest{
		System:      []string{n.prompt},
		Messages:    []llm.Message{llm.UserText(string(user))},
		Tools:       []llm.ToolSchema{n.schema},
		MaxTokens:   n.maxTokens,
		Temperature: &temp,
		Thinking:    "disabled",
	}
	acc := llm.NewAccumulator()
	var streamErr error
	for ev, err := range prov.Stream(cctx, req) {
		if err != nil {
			streamErr = err
			break
		}
		acc.Add(ev)
	}
	if streamErr != nil {
		if errors.Is(streamErr, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return NormalizerResult{}, fmt.Errorf("%w: 模型调用超时（上限 %s）", ErrNormalizerUnavailable, n.timeout)
		}
		return NormalizerResult{}, fmt.Errorf("%w: 模型调用失败: %v", ErrNormalizerUnavailable, streamErr)
	}
	if err := cctx.Err(); err != nil {
		return NormalizerResult{}, fmt.Errorf("%w: 模型调用超时（上限 %s）", ErrNormalizerUnavailable, n.timeout)
	}

	uses := acc.Message().ToolUses()
	if len(uses) == 0 {
		return NormalizerResult{}, fmt.Errorf("%w: 模型没有调用 %s 工具", ErrNormalizerMalformed, n.schema.Name)
	}
	if len(uses) > 1 {
		return NormalizerResult{}, fmt.Errorf("%w: 模型调用了 %d 个工具，契约要求恰好一个 %s", ErrNormalizerMalformed, len(uses), n.schema.Name)
	}
	if uses[0].Name != n.schema.Name {
		return NormalizerResult{}, fmt.Errorf("%w: 模型调用了 %s，契约要求 %s", ErrNormalizerMalformed, uses[0].Name, n.schema.Name)
	}

	var raw normalizerToolOutput
	if err := json.Unmarshal(uses[0].Input, &raw); err != nil {
		return NormalizerResult{}, fmt.Errorf("%w: 工具入参不是合法 JSON: %v", ErrNormalizerMalformed, err)
	}
	out := NormalizerResult{
		StructuredIntent: raw.StructuredIntent,
		AnomalyFlag:      raw.AnomalyFlag,
		AnomalyReason:    raw.AnomalyReason,
	}
	if out.StructuredIntent.ActionType == "" {
		// ⚠️ 这一条同时覆盖两种失败：① JSON 里没有 structured_intent；
		// ② 有 structured_intent 但没有 action_type。
		// 另外③「入参不是合法 JSON」也会落到这里：llm.Accumulator 会把非法
		// JSON 兜底成 {}（见 third_party/norma/llm/accumulate.go 的 Message），
		// 于是反序列化成功但字段全空 —— 结果一样是「不合规」，一样拒绝。
		return NormalizerResult{}, fmt.Errorf("%w: 输出缺少 structured_intent / action_type", ErrNormalizerMalformed)
	}
	if out.StructuredIntent.RawPayloadDigest == "" {
		return NormalizerResult{}, fmt.Errorf("%w: 输出缺少 raw_payload_digest", ErrNormalizerMalformed)
	}
	if out.StructuredIntent.ActionType == actionTypeUnparseable {
		// 原始动作根本解析不出来 ⇒ 没有可信的结构化意图 ⇒ 拒绝。
		// 提示词原文（agent/prompts/intent-normalizer.md:103-106）要求「交给
		// policy gate 决定」，但闸门的裁决对 unparseable 同样是拒绝 ⇒ 落在
		// 这里不会改变结局，只是把原因码落在更准确的一侧（输出不合规，
		// 而不是「越界」）。
		return NormalizerResult{}, fmt.Errorf("%w: 原始动作无法解析（action_type=unparseable）", ErrNormalizerMalformed)
	}
	if !knownActionTypes[out.StructuredIntent.ActionType] {
		return NormalizerResult{}, fmt.Errorf("%w: 未知的 action_type=%q", ErrNormalizerMalformed, out.StructuredIntent.ActionType)
	}
	if requiresTarget(out.StructuredIntent.ActionType, in.RawPayload) && out.StructuredIntent.TargetHint == "" {
		return NormalizerResult{}, fmt.Errorf("%w: 动作类型 %s 需要目标线索，但输出里没有 target_hint", ErrNormalizerMalformed, out.StructuredIntent.ActionType)
	}

	// source_agent / task_id 由规整器自己盖上：模型说的不算数（它可能复述错，
	// 也可能被原始动作里的文本诱导）。它们用于缓存键与审计归属。
	out.SourceAgent = in.SourceAgent
	out.TaskID = in.TaskID
	return out, nil
}

// normalizerToolOutput 是**模型工具入参**的解码目标。
//
// 为什么不是 NormalizerResult：NormalizerResult 同时扮演两个角色 ——
// ①「模型工具入参的形状」；②「规整器的产出」。这两个角色的字段类型并不一致：
// tools 表的 emit_structured_intent 行把 task_id 声明成 **string**（提示词的
// 输出模板也是 `"task_id": "<id>"`），而 NormalizerResult 把它声明成 int64。
// 模型遵守 schema 给出 "task_id":"7" ⇒ json.Unmarshal 整包失败 ⇒ 一次完全
// 合法的工具调用被判成 normalizer_malformed，SoD 链对每一次调用都拒绝。
//
// 所以解码目标只列**代码真正要用的字段**：structured_intent（要校验）、
// anomaly_flag / anomaly_reason（要落审计与闸门）。task_id / source_agent
// 不在其中 —— 它们本来就是「模型说了不算」的字段（Normalize 末尾用规整器
// 自己的上下文覆盖），模型给成字符串、数字还是干脆不给，都不改变任何裁决。
//
// ⚠️ 这**不是**放宽校验：structured_intent 的每一条校验（action_type 已知、
// raw_payload_digest 在、目标线索在、unparseable 拒）一条都没少。放开的只是
// 一个代码根本不用、且类型由外部（tools 表）决定的元数据字段。
type normalizerToolOutput struct {
	StructuredIntent StructuredIntent `json:"structured_intent"`
	AnomalyFlag      bool             `json:"anomaly_flag"`
	AnomalyReason    string           `json:"anomaly_reason"`
}

// rawPayloadForPrompt 把原始载荷放进提示词输入。
//
// 能解成 JSON 就按 JSON 放（保留结构，模型看得更准）；解不动就按字符串放。
// 为什么不用 json.RawMessage 直接塞：那会让 json.Marshal 失败（非法 JSON 的
// RawMessage 会让整个请求构造不出来）⇒ 一个畸形载荷就能把规整器变成「永远
// 不可用」。
func rawPayloadForPrompt(payload []byte) any {
	if len(payload) == 0 {
		return ""
	}
	if json.Valid(payload) {
		return json.RawMessage(payload)
	}
	return string(payload)
}

// inferActionKind 把工具名映射成规整器的 raw_action_kind。
//
// ⚠️ 映射是【代码常量】，不是模型判断。未知工具 ⇒ tool_call
// （最保守的一类：它仍然会被 scope / tier2 检查管住）。
func inferActionKind(toolName string) string {
	switch toolName {
	case "Bash", "shell_open", "shell_send", "Task", "Grep", "Glob":
		return "shell"
	case "WebFetch", "web_search":
		return "http"
	default:
		return "tool_call"
	}
}

// requiresTarget 报告该动作类型是否必须有目标线索。
//
// 规则是【代码常量】（与 internal/sodpolicy 同一纪律：能被外部数据改写的判定
// 等于没有判定）：
//
//   - http_request：请求必然有 host ⇒ 缺 target_hint 就是转换不完整。
//   - command_execution：当原始载荷里出现**明确的目标标记**（URL 方案前缀或
//     IP 字面量）时，提示词要求规整器提取候选目标；此时缺 target_hint 就是
//     漏提取 ⇒ 不合规。
//   - tool_invocation：工具是否声明了 target 参数，规整器看不到工具元数据
//     （它只拿到原始入参）⇒ 不在此判定，交给闸门的范围检查（空目标 ⇒ 越界
//     拒绝）。
//
// 为什么 shell 不做「看起来像域名」的模糊判断：`cat /var/log/app.log` 里那个
// 带点的词不是目标；把它当成目标会让规整器必须为一条纯本地命令提取
// target_hint，提取不出来就被判不合规 —— 一个本地读文件命令被无理由拒绝，
// 比漏检严重得多。
func requiresTarget(actionType string, rawPayload []byte) bool {
	switch actionType {
	case actionTypeHTTPRequest:
		return true
	case actionTypeCommandExecution:
		return targetMarkerRe.Match(rawPayload)
	default:
		return false
	}
}

// targetMarkerRe 匹配「明确的目标标记」：URL 方案前缀或 IPv4 字面量。
var targetMarkerRe = regexp.MustCompile(`(?i)(?:https?|ftp)://|\b\d{1,3}(?:\.\d{1,3}){3}\b`)
