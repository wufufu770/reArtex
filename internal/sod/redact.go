package sod

import (
	"regexp"
	"unicode/utf8"
)

// redactReplacement 是脱敏命中处的占位文本。
//
// 为什么用带尖括号的词而不是空串：空串会让「这里原来有一个值」变成「这里本来
// 就没东西」，事后看预览的人会以为载荷里没有这个字段 —— 占位文本必须让「被抹掉」
// 这件事本身是可见的。
const redactReplacement = "«redacted»"

// redactPatterns 是脱敏规则（**代码常量**：不读配置、不读 DB、不来自模型）。
//
// 规则清单（与 internal/sodpolicy 的纪律一致 —— 能被外部数据改写的脱敏规则等于
// 没有脱敏）：
//
//  1. 认证方案 + 凭据（Bearer xxx / Basic xxx）
//  2. Authorization / Cookie 一类的头
//  3. 形如 *_TOKEN / *_PASSWORD / *_SECRET / *_KEY / *_APIKEY 的键
//  4. password= / passwd= / pwd= / token= / secret= / api_key= 形式的赋值
//  5. 连续 32 位以上的 Base64 串
//  6. 连续 32 位以上的十六进制串
//
// 顺序有意义：规则 1 必须排在规则 2 之前 —— `Authorization: Bearer <secret>`
// 里规则 2 只能吃掉 `Authorization: Bearer`（值以空格分隔），剩下的 `<secret>`
// 会漏出去；先把「方案 + 凭据」整段吃掉，规则 2 再把剩下的头部整体抹掉。
//
// ⚠️ 脱敏是**纵深防御**，不是保证：正则不可能覆盖所有形态（自制的编码、拼在
// 句子里的口令……）。真正的保证是「原文根本不入库」—— 预览只是给人看的线索。
var redactPatterns = []*regexp.Regexp{
	// ① 认证方案 + 凭据（先于头部规则执行，理由见上）。
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`),
	// ② 认证 / Cookie 类头部。
	regexp.MustCompile(`(?i)(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api-key)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&]+)`),
	// ③ 名字里带敏感词的键（TOKEN / PASSWORD / SECRET / KEY / APIKEY）。
	regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(token|password|passwd|secret|apikey|api_key|private_key)[A-Za-z0-9_]*)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;&]+)`),
	// ④ 查询串 / 命令行里的敏感赋值。
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api_key)\s*=\s*([^\s&;"']+)`),
	// ⑤ 长 Base64 串（密钥、JWT、证书）。
	regexp.MustCompile(`\b[A-Za-z0-9+/]{32,}={0,2}\b`),
	// ⑥ 长十六进制串（摘要、令牌）。
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
}

// RedactPreview 返回脱敏后的预览（最多 maxBytes 字节）。
//
// 顺序是**先脱敏、后截断**：反过来的话，一个恰好横跨截断边界的凭据会被切成
// 「前半截留在预览里、后半截被丢掉」—— 那等于泄密。脱敏在前，整段匹配都被替换
// 掉，边界落在哪里都不影响「原文不在预览里」这件事。
//
// maxBytes <= 0 ⇒ 空串（调用方不想留预览）。
//
// ⚠️ 脱敏是纵深防御：真正的保证是「原始载荷只以摘要入库」（见 CanonicalDigest）。
func RedactPreview(payload []byte, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	redacted := payload
	for _, re := range redactPatterns {
		redacted = re.ReplaceAll(redacted, []byte(redactReplacement))
	}
	return truncateUTF8(redacted, maxBytes)
}

// truncateUTF8 按字节上限截断，但不把多字节字符切成半个。
//
// 为什么在意这个：预览会被人读，也会被再序列化；半个 UTF-8 字符会让读预览的人
// 看到乱码，还会让下游的 JSON 解析失败。回退到字符边界只损失几个字节。
func truncateUTF8(b []byte, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(b) <= maxBytes {
		return string(b)
	}
	cut := b[:maxBytes]
	// 最后一个字节若是 UTF-8 续字节（0b10xxxxxx），说明切在了字符中间 ⇒ 回退。
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return string(cut)
}
