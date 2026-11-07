// 群组预算的**估算**半边：这次模型调用大概要花多少 token。
//
// 为什么单独一个文件：预扣（reserve）这条链路有两个互不相干的关注点 ——
// 「估多少」（本文件）与「扣不扣得到」（quota.go）。拆开是因为估算这一侧
// 改动的理由（新的内容类型、新的重试口径）与闸门那一侧（账本状态机）完全不同：
// 混在一个文件里，改估算就会顺手碰到钱的记账。
//
// 关键口径：
//
//	估算不出来 ⇒ 报错，调用方按 fail-closed 拒绝这次调用（见 checkEstimable）。
//	估得出来 ⇒ 只是一个估算：预扣按它扣，结算按真实 usage 调整，差额结清 ——
//	估算值永远不会被当成最终用量。
package llmrec

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Autumn-27/norma/llm"
)

// asciiCharsPerToken 是 ASCII 文本"字符数/token"的经验值（4）。
const asciiCharsPerToken = 4

// defaultOutputTokenEstimate 是请求未指定 max_tokens 时的输出侧估算，同时也是
// 输出侧估算的**上限**（maxOutputTokenEstimate）。
//
// 为什么要有上限（方案 v3 缺陷 C1）：请求侧的 MaxTokens 是「模型允许写多长」的
// 上限（常见 32K），不是「这次会写多长」的期望。按上限整额预扣 × 重试预算，
// 一次调用就预扣掉 98K+ —— 120K 的群组预算并发容量直接变成 1，planner+worker
// 全部被假性拒绝。输出侧估算改成「未指定按 4096，指定了也不超过 4096」：
//   - 长输出场景被低估 ⇒ 结算按真实 usage 调整（预扣只是并发闸，不是计费）；
//   - 超预算的真实用量由结算负差额与账本口径兜住，这里不为它预支整个预算。
const defaultOutputTokenEstimate = 4096

// DefaultRetryBudget 是预扣时假定的「provider 内部重试」预算（重试次数）。
//
// 为什么需要它：闸门看到的是**一次**对外调用，而 provider 内部还有一层自动
// 重试（建连重试 / 空响应重试，见 agent.RetryConfig 与 docs/LLM重试设计.md）——
// 那层重试对闸门**不可见** ⇒ 一次预扣可能对应多次真实请求 ⇒ 只按一次估算
// 预扣会低估预算（最坏的情况是几次重试全部计费，而账上只扣了一次）。
//
// 取值口径：内建的保守下界（2 次重试 ⇒ 最多 3 次尝试）。**不是**精确的
// profile 重试上限 —— profile 的重试配置在 server 层解析（server/llmretry.go
// 叠全局策略与 profile 覆盖），闸门这一层拿不到它。在真实上限接进来之前，
// 先按这个够用的常量兜住「低估」这个更严重的方向：少扣会击穿预算，多扣只是
// 暂时占住额度（结算时按真实用量整块退回）。
const DefaultRetryBudget = 2

// QuotaEstimate 是一次模型调用的 token 估算（输入侧估算 + 输出侧上限）。
type QuotaEstimate struct {
	TotalTokens  int64
	InputTokens  int64
	OutputTokens int64
}

// EstimateTokens 估算一次模型调用要花的 token：输入侧估算 + 输出侧上限。
//
// 重试预算只乘**输入侧**（方案 v3 缺陷 C1）：重试重发的是整份输入，输出是每次
// 独立产生的——但输出侧已按 4096 上限估，若再乘 attempts，一次 32K MaxTokens
// 的调用会预扣 98K+，把普通群组预算的并发压成 1（假性额度耗尽的根因）。
// 取舍：输出侧按单次 4096 上限计入（长输出被低估的部分由结算按真实 usage
// 调整兜底——预扣只是并发闸，不是计费）。
//
// 估不出来（checkEstimable 拒绝的请求形态）必须**返回错误**：调用方按
// fail-closed 拒绝这次调用。绝不能退回一个「尽力而为」的数 —— 那正是
// 预算闸门被整条跳过、预算形同虚设的漏洞。
func EstimateTokens(req llm.CompletionRequest, maxRetries int) (QuotaEstimate, error) {
	if err := checkEstimable(req); err != nil {
		return QuotaEstimate{}, err
	}
	input := estimateInputTokens(req)
	output := int64(req.MaxTokens)
	if output <= 0 || output > defaultOutputTokenEstimate {
		output = defaultOutputTokenEstimate
	}
	attempts := int64(maxRetries) + 1
	if attempts < 1 {
		attempts = 1
	}
	total := input*attempts + output
	if total <= 0 {
		return QuotaEstimate{}, errors.New("无法估算本次模型请求的 token")
	}
	return QuotaEstimate{
		TotalTokens:  total,
		InputTokens:  input * attempts,
		OutputTokens: output,
	}, nil
}

// checkEstimable 是「这类请求能不能估」的**白名单**。
//
// 为什么是白名单而不是黑名单：黑名单（列出不能估的类型）要求我们**已经知道**
// 未来会出现哪些类型；白名单只要求知道现在有哪些。两种漏法的代价也不同：
//   - 白名单漏掉某个文本类 ⇒ 那类请求全被拒（fail-closed 的正确表现，
//     代价是功能不可用）；
//   - 黑名单漏掉某个新类型 ⇒ 估算按 0 算 ⇒ 额度低估（**更严重**：预算被击穿）。
//
// 所以取白名单：宁可拒，不可低估。
//
// 这道检查是代码常量而不是配置项：能不能估属于「钱花不花得出去」这条边界，
// 放进可热改的配置就等于让任何能改配置的人给自己开一条免检通道。
func checkEstimable(req llm.CompletionRequest) error {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if !isEstimableBlock(b) {
				return fmt.Errorf("额度估算不支持该请求内容类型（已拒绝调用）")
			}
		}
	}
	return nil
}

// isEstimableBlock 判断一个内容块是不是「可估算」的文本类。
//
// 放行的是**明确的**文本 / 思考 / 工具调用 / 工具结果四类（norma/llm/types.go
// 里 BlockType 的全部取值）。图片、音频、附件，以及将来新增的任何未知类型
// 一律视为不可估算 —— 对未知类型按 0 算等于凭空少扣预算。
func isEstimableBlock(b llm.ContentBlock) bool {
	switch b.Type {
	case llm.BlockText, llm.BlockThinking, llm.BlockToolUse, llm.BlockToolResult:
		return true
	default:
		return false
	}
}

// estimateInputTokens 估算输入侧 token：system + 历史消息 + 工具定义。
func estimateInputTokens(req llm.CompletionRequest) int64 {
	var total int64
	add := func(s string) { total += countTokens(s) }
	for _, seg := range req.System {
		add(seg)
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case llm.BlockText:
				add(block.Text)
			case llm.BlockThinking:
				add(block.Thinking)
			case llm.BlockToolUse:
				add(block.Name)
				add(string(block.Input))
			case llm.BlockToolResult:
				add(block.ToolUseID)
				for _, sub := range block.Content {
					add(sub.Text)
					add(sub.Thinking)
				}
			}
		}
	}
	for _, tool := range req.Tools {
		add(tool.Name)
		add(tool.Description)
		if raw, err := json.Marshal(tool.InputSchema); err == nil {
			add(string(raw))
		}
	}
	return total
}

// countTokens 估算一段文本的 token 数：ASCII 每 4 个字符算 1 个 token，
// 非 ASCII（中文等）每个字符约 1 个 token。
func countTokens(s string) int64 {
	var ascii, wide int64
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return (ascii+asciiCharsPerToken-1)/asciiCharsPerToken + wide
}
