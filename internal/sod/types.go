// Package sod 实现 worker 工具执行路径上的职责分离（SoD）链。
//
// 铁律：**模型只提议，确定性层裁决**。
//   - NormalizerResult 是【模型产出】—— 不可信，只作结构化线索。
//   - Verdict 是【代码产出】—— 唯一的裁决来源。
//
// 任何「让模型参与裁决」的改法都是对本包的破坏。
//
// 为什么这条铁律写在这里而不是散在各文件：包里同时住着「模型说的话」与
// 「代码说的话」。这两种数据一旦混在一个结构体里，调用方迟早会把模型的
// 布尔字段当判据用 —— 而模型的布尔字段是可被提示词注入改写的。放在包注释
// 里，是为了让每个进这个包的人第一眼就看见这条边界。
package sod

// StructuredIntent 是规整器的产出（对应 agent/prompts/intent-normalizer.md 的输出格式）。
//
// ⚠️ 它是**模型产出的数据**，不是裁决依据：所有布尔字段都不可信，
// 闸门只把它当作「结构化线索」。
type StructuredIntent struct {
	ActionType       string        `json:"action_type"`
	Executable       string        `json:"executable"`
	Arguments        []string      `json:"arguments"`
	TargetHint       string        `json:"target_hint"`
	ShellFeatures    ShellFeatures `json:"shell_features"`
	BodyDigest       string        `json:"body_digest"`
	RawPayloadDigest string        `json:"raw_payload_digest"`
}

// ShellFeatures 是命令行的结构特征（管道 / 重定向 / 命令替换 ……）。
//
// 为什么要单独列出来：这些特征是「原始动作是否超出一次简单调用」的客观
// 描述，比「模型觉得这条命令危险」更难被糊弄 —— 它们是模型读到的字符
// 形态的转述，闸门据此决定要不要多问一句。
type ShellFeatures struct {
	HasPipe                bool `json:"has_pipe"`
	HasRedirect            bool `json:"has_redirect"`
	HasCommandSubstitution bool `json:"has_command_substitution"`
	HasBackground          bool `json:"has_background"`
	HasObfuscation         bool `json:"has_obfuscation"`
}

// NormalizerResult 是规整器的完整产出。
//
// SourceAgent / TaskID 是**规整器自己记下的上下文**，用于让缓存键与审计能
// 追到「这条线索是在谁的什么任务里产生的」；它们不参与闸门判定。
type NormalizerResult struct {
	StructuredIntent StructuredIntent `json:"structured_intent"`
	AnomalyFlag      bool             `json:"anomaly_flag"`
	AnomalyReason    string           `json:"anomaly_reason"`
	SourceAgent      string           `json:"source_agent"`
	TaskID           int64            `json:"task_id"`
}

// Verdict 是闸门的裁决（**只有代码能产出它**）。
//
// Checks 是五道检查各自的结果（检查名 → 结论），留痕用；它不参与判定 ——
// 判定早已由闸门按固定短路顺序做完。
type Verdict struct {
	Verdict      string            `json:"verdict"` // approve | deny | needs_review
	ReasonCode   string            `json:"reason_code"`
	Checks       map[string]string `json:"checks"`
	TokenID      string            `json:"-"` // token 本体，永不序列化
	TokenDigest  string            `json:"token_digest"`
	IntentDigest string            `json:"structured_intent_digest"`
	Note         string            `json:"note"`
}
