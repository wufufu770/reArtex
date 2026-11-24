package db

// 事件 payload 消毒（v2.1 统一架构优化增补 §0.12，0.15.70，批次 TI-12）。
//
// §0.12 原文口径：「事件 outbox 不得将凭据、Cookie、Authorization header 或
// 原始敏感正文写入普通事件 payload；使用受控 evidence_ref。」
//
// 执法点选择：消毒在 **db.EnqueueEvent（存储单点）** 而不是各生产方——生产方
// 会忘，存储层不会；放在 db 包还避免了 events↔db 的 import 环（events 的派发器
// 已依赖 db）。规则是确定性代码（INV-10），不由 prompt 决定。
//
// 三条确定性规则：
//  1. 凭据类键（EventCredentialKeys）的叶子字符串值一律改写为占位符——键名
//     保留（事件语义常需要「哪个 header 被记录了」），值不留；
//  2. payload 超过 MaxEventPayloadBytes 整体拒绝——大正文属于 evidence_refs
//     受控引用模式（0.15.63），事件只传引用与 hash；
//  3. 非 JSON payload 整体拒绝——消毒面建立在可解析结构上，二进制/自由文本
//     走证据引用，不进事件。
//
// 红action 可审计：被改写的键路径记录进 payload 的 `_sanitized` 标记块，
// 与行内 last_error 一起构成观测面。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxEventPayloadBytes 事件 payload 上限（§0.12：原始敏感正文与大内容不进事件）。
const MaxEventPayloadBytes = 64 << 10 // 64 KiB

// redactedPlaceholder 凭据类叶子值的占位符（指向受控引用的语义提示）。
const redactedPlaceholder = "[redacted:use evidence_ref]"

// EventCredentialKeys 凭据类键白名单（小写比较，深度嵌套同规则）。
// 覆盖 §0.12 点名的 Cookie/Authorization，外加常见凭据键形。键名匹配是
// 确定性清单，不是启发式——新增键形要过评审（宁可漏一个新增形，也不做
// 猜测式拦截把正常事件打成筛子）。
var EventCredentialKeys = map[string]bool{
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"password":            true,
	"passwd":              true,
	"secret":              true,
	"api_key":             true,
	"apikey":              true,
	"api-key":             true,
	"token":               true,
	"access_token":        true,
	"refresh_token":       true,
	"bearer":              true,
	"private_key":         true,
	"session_cookie":      true,
}

// SanitizeEventPayload 消毒一条事件 payload：
//   - 凭据类键的叶子字符串值 → 占位符（键路径记入返回值 redacted）；
//   - payload 超限 / 非 JSON → 错误（调用方整体拒绝该事件）。
//
// 返回 clean 是消毒后的字节；redacted 是被改写的键路径（a.b 形式）。
func SanitizeEventPayload(payload []byte) (clean []byte, redacted []string, err error) {
	if len(payload) > MaxEventPayloadBytes {
		return nil, nil, fmt.Errorf("事件 payload %d 字节超上限 %d——大内容走 evidence_refs 受控引用，不进事件",
			len(payload), MaxEventPayloadBytes)
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, nil, fmt.Errorf("事件 payload 不是合法 JSON（消毒面建立在可解析结构上）: %w", err)
	}
	san := &eventSanitizer{}
	cleanRoot := san.walk(root, nil)
	if len(san.redacted) > 0 {
		// 显式消毒标记：观测面知道这条事件被改写过、改了哪些键。
		cleanRoot = map[string]any{
			"_sanitized": map[string]any{"redacted_keys": san.redacted},
			"payload":    cleanRoot,
			"sanitized":  true,
		}
	}
	out, err := json.Marshal(cleanRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("消毒后序列化失败: %w", err)
	}
	return out, san.redacted, nil
}

type eventSanitizer struct {
	redacted []string
}

// walk 递归消毒：map 键名命中凭据清单 → 叶子字符串值替换占位符（保留结构）。
func (s *eventSanitizer) walk(v any, path []string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if EventCredentialKeys[strings.ToLower(k)] {
				s.redacted = append(s.redacted, strings.Join(append(path, k), "."))
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = s.walk(val, append(path, k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = s.walk(val, path) // 数组元素键路径不含下标（同一规则语义）
		}
		return out
	default:
		return v
	}
}
