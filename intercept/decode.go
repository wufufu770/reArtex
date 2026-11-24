package intercept

// Audit #42 移植（0.15.74）：入参匹配面的全量递归解码。
//
// 原口径（0.11.x）只取 9 个已知顶层字段（command/text/pattern/…），未知键名下的
// 字符串（如 {"argv":{"0":"\u0063url …"}}）整体漏出匹配面。本文件把解码改为对整个
// 已解码 JSON 值的递归 walk：全部字符串值（含嵌套对象/数组、键名、以及字符串内再
// 嵌一层 JSON 文本的双重编码）都进规范化文本。原始未转义文本照旧同时参与匹配
// （ruleMatches 的 raw + normalized 双保险），解码只可能【加宽】匹配面，不收窄。
//
//   - 解码失败（非法 JSON）回落原文匹配（NormalizeToolInput 返回空串，行为不变）；
//   - 结构深度上限 maxDecodeDepth：恶意深嵌套到顶即停，残余形态仍有 raw 兜底；
//   - 字符串内的转义层解链沿用 decodeEscapedString（maxDecodeLayers，另一维度）。

import (
	"encoding/json"
	"strings"
)

// maxDecodeDepth 是结构递归的层数上限。真实 payload 的结构嵌套两三层到头；
// 上限只为防恶意深嵌套在匹配热路径上兜圈，到顶即停（该子树不再展开，不产生
// 误放行——raw 原文始终参与匹配）。
const maxDecodeDepth = 8

// decodeStringsRecursive 对已解码的 JSON 值做递归 walk，收集匹配面的全部文本：
//   - 字符串：经 decodeValue 解开转义层后收录；若其本身是 JSON 文本（以 { 或 [
//     开头），再解一层结构（双重编码），深度 +1；
//   - 对象/数组：递归每个值；键名也收录（规则可能匹配键）；
//   - 其他标量（数字/布尔）：按 JSON 字面收录——历史上 9 字段口径会把这些值
//     re-marshal 进规范化文本，收录它们保证存量规则匹配面只加宽不收窄。
func decodeStringsRecursive(v any, depth int, out *[]string) {
	if depth > maxDecodeDepth {
		return
	}
	switch t := v.(type) {
	case string:
		s, _ := decodeValue(t).(string)
		*out = append(*out, s)
		trimmed := strings.TrimSpace(s)
		if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
			var nested any
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				decodeStringsRecursive(nested, depth+1, out)
			}
		}
	case map[string]any:
		for k, val := range t {
			*out = append(*out, k)
			decodeStringsRecursive(val, depth+1, out)
		}
	case []any:
		for _, val := range t {
			decodeStringsRecursive(val, depth+1, out)
		}
	default:
		if b, err := json.Marshal(v); err == nil {
			*out = append(*out, string(b))
		}
	}
}
