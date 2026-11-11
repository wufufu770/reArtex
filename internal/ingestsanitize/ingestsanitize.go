// Package ingestsanitize 是外部数据入库前的消毒链（T7，方案 v10 §6 三期，
// 链序照 d2d：fail-closed）。
//
// 外部来源（MCP 拉取的资产、web 搜索结果、目标页正文）是【不可信输入】：其中
// 可能藏提示注入（诱导 agent 忽略既有指令）、控制字符（污染终端/日志/检索）。
// 链序：
//
//	① 空 / 超长（1MB）→ 丢（dropped）；
//	② 注入扫描：HIGH 特征（明确的指令注入/泄漏试探）→ 丢（dropped）；
//	   SOFT 特征（可疑但模糊）→ 放行 + 告警（warned）；
//	③ 消毒：剥离控制字符 / 零宽字符 / 双向文本控制符（任何路径都执行）；
//	④ 返回时附 [external:<src>] 标记要求（Marker）——下游展示/落库时必须
//	   带上来源标记，外部内容不许伪装成平台自己的话。
//
// 拒绝计数（Refused）按来源累计，进程内观测口径（log 之外的数字留痕）。
package ingestsanitize

import (
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// MaxBytes 是单条外部数据的长度上限（1MB）。超长不是「更多数据」，是「更难
// 审的数据」——外部来源没有正当理由携带 1MB 的文本进资产/事实库。
const MaxBytes = 1 << 20

// 动作结论（action 返回值）：
const (
	// ActionOK 表示只做了消毒（控制字符剥离），无注入特征。
	ActionOK = "ok"
	// ActionWarned 表示命中 SOFT 特征：放行（已消毒）+ 告警标记。
	ActionWarned = "warned"
	// ActionDropped 表示丢弃（空/超长/HIGH 注入特征），clean 为 nil。
	ActionDropped = "dropped"
)

// Sanitize 是消毒链主入口。src 是来源标识（如 "scopesentry"/"insert_assets"/
// "web_search"），进错误文案与 Marker。raw 为空、超长、命中 HIGH 特征时返回
// (nil, "dropped", err)；命中 SOFT 特征时返回 (clean, "warned", nil)；其余
// 返回 (clean, "ok", nil)。clean 保证是合法 UTF-8 且不含控制/零宽字符。
func Sanitize(src string, raw []byte) ([]byte, string, error) {
	// ① 空/超长 → 丢。
	if len(raw) == 0 {
		refuse(src)
		return nil, ActionDropped, fmt.Errorf("[external:%s] 空内容，丢弃", src)
	}
	if len(raw) > MaxBytes {
		refuse(src)
		return nil, ActionDropped, fmt.Errorf("[external:%s] 内容超长（%d > %d 字节上限），丢弃", src, len(raw), MaxBytes)
	}
	s := string(raw)
	if !utf8.Valid(raw) {
		// 非法 UTF-8 按 rune 替换（丢整条太狠——很多来源只是给半个多字节字符），
		// 替换后继续走扫描与消毒。
		s = strings.ToValidUTF8(s, "?")
	}
	// ② 注入扫描（HIGH 丢 / SOFT 告警）。
	if reason, high := scanHigh(s); high {
		refuse(src)
		return nil, ActionDropped, fmt.Errorf("[external:%s] 命中注入特征（%s），丢弃", src, reason)
	}
	soft := scanSoft(s)
	// ③ 消毒：剥离控制字符/零宽字符/双向控制符。
	clean := []byte(stripControls(s))
	if soft {
		warn(src)
		return clean, ActionWarned, nil
	}
	return clean, ActionOK, nil
}

// Marker 是外部内容的来源标记（④）：下游展示/落库时必须附上，外部内容不许
// 伪装成平台自己的话。
func Marker(src string) string { return "[external:" + src + "]" }

// ---------- 拒绝/告警计数（log 之外的数字留痕）----------

var refused, warned atomic.Uint64

func refuse(src string) { refused.Add(1) }
func warn(src string)   { warned.Add(1) }

// Refused 返回累计丢弃条数（所有来源合计；进程内观测口径）。
func Refused() uint64 { return refused.Load() }

// Warned 返回累计告警放行条数。
func Warned() uint64 { return warned.Load() }

// ---------- ② 注入扫描 ----------

// highPatterns 是 HIGH 特征：明确的提示注入/泄漏试探。命中即丢（fail-closed）。
// 特征来自 d2d 的注入清单口径：忽略前缀指令、ignore previous、system prompt
// 泄漏试探、</system> 闭合标签、工具调用 JSON 注入形态。
var highPatterns = []string{
	// 忽略前缀指令（中英常见变体）。
	"忽略之前的指令", "忽略之前指令", "忽略上述指令", "忽略以上指令",
	"无视之前的指令", "无视上述指令", "忽略前面的指令", "忽略先前指令",
	"忽略所有指令", "无视所有指令", " disregarding previous", // 带前导空格降低误伤
	// ignore previous / disregard above（英文高频注入头）。
	"ignore previous", "ignore prior instructions", "ignore all previous",
	"disregard previous", "disregard the above", "disregard above",
	"forget previous instructions", "new instructions:",
	// system prompt 泄漏试探。
	"system prompt", "your system prompt", "print your instructions",
	"reveal your instructions", "repeat your instructions",
	"输出你的系统提示词", "打印系统提示词", "泄露系统提示词", "复述你的指令",
	// </system> 闭合标签（伪造 system 段边界）。
	"</system>",
	// 工具调用 JSON 注入形态（把工具调用伪装成数据内容）。
	`"tool_calls"`, `"function_call"`, `"type":"tool_use"`, `"type": "tool_use"`,
}

// softPatterns 是 SOFT 特征：可疑但模糊（可能是注入脚手架，也可能是正常内容）。
// 放行 + 告警，不丢。非常规 unicode 控制符与超长无结构行走 scanSoft 的 rune
// 判定（串模式抓不住它们）。
var softPatterns = []string{
	"<|im_start|>", "<|im_end|>", "[inst]", "[/inst]", "###instruction",
	"assistant:", "你现在是", "从现在开始你是", "新指令：", "新指令:",
	"act as", "pretend to be", "roleplay as",
}

// scanHigh 返回 (命中特征, true) 或 ("", false)。大小写不敏感。
func scanHigh(s string) (string, bool) {
	low := strings.ToLower(s)
	for _, p := range highPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return p, true
		}
	}
	return "", false
}

// scanSoft 报告是否命中 SOFT 特征：模糊的注入脚手架串、非常规 unicode 控制
// 符（C1 控制段、双向文本控制符）、超长无结构行。
func scanSoft(s string) bool {
	low := strings.ToLower(s)
	for _, p := range softPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	const (
		maxControlRunes = 4    // C1/双向控制符出现 4 个以上才算可疑（1-2 个可能是脏数据）
		structuredLine  = 4000 // 单行超过这个长度且几乎无空白 = 超长无结构行
	)
	controls, lineLen, wsInLine := 0, 0, 0
	for _, r := range s {
		if r == '\n' || r == '\r' {
			if lineLen > structuredLine && wsInLine == 0 {
				return true
			}
			lineLen, wsInLine = 0, 0
			continue
		}
		lineLen++
		if unicode.IsSpace(r) {
			wsInLine++
		}
		if isSuspiciousControl(r) {
			controls++
			if controls >= maxControlRunes {
				return true
			}
		}
	}
	if lineLen > structuredLine && wsInLine == 0 {
		return true
	}
	return false
}

// isSuspiciousControl 报告 rune 是否属非常规控制符：C1 控制段（U+0080-U+009F，
// 正常文本不该有）与双向文本控制符（U+202A-U+202E、U+2066-U+2069，可伪装
// 展示顺序）。零宽字符单独算消毒对象（③ 剥离），不算 SOFT 特征。
func isSuspiciousControl(r rune) bool {
	if r >= 0x80 && r <= 0x9F {
		return true
	}
	if r >= 0x202A && r <= 0x202E { // LRE/RLE/PDF/LRO/RLO
		return true
	}
	if r >= 0x2066 && r <= 0x2069 { // LRI/RLI/FSI/PDI
		return true
	}
	return false
}

// ---------- ③ 消毒 ----------

// stripControls 剥离控制字符与零宽字符，保留可读结构（\t \n \r 空格系）。
// 返回值保证不含：Cc（除 \t\n\r）、Cf 的零宽/双向成员、非字符（替换为 '?'）。
func stripControls(s string) string {
	if !strings.ContainsFunc(s, needsStrip) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(r)
		case r == utf8.RuneError:
			b.WriteByte('?')
		case isZeroWidth(r) || (unicode.IsControl(r) && r < 0x80):
			// 零宽字符与 ASCII 控制符：剥离（不替换，不留痕迹位）。
		case unicode.IsControl(r):
			b.WriteByte('?') // 其余控制符（C1 残余）替换为 '?'，保留长度线索
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isZeroWidth 报告 rune 是否零宽字符（U+200B-U+200F、U+2060-U+2064、U+FEFF、
// U+00AD 软连字符）。它们不可见但会改变检索/比较语义，入库前一律剥离。
func isZeroWidth(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF || r == 0x00AD
}

// needsStrip 是 stripControls 的快速预判（无脏字符时零拷贝返回）。
func needsStrip(r rune) bool {
	return isZeroWidth(r) || isSuspiciousControl(r) || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r') || r == utf8.RuneError
}
