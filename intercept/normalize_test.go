package intercept

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func reFor(t *testing.T, pattern string) compiledRule {
	t.Helper()
	c := compiledRule{InterceptRule: db.InterceptRule{
		Name: "t", MatchTarget: "tool_input", MatchType: "regex", Pattern: pattern,
	}}
	c.re = regexp.MustCompile(pattern)
	return c
}

func TestNormalizeUnblocksUnicodeEscapeBypass(t *testing.T) {
	// norma 不解码入参，原始字节里没有 'r'，词表正则不命中；解码后必须命中。
	raw := []byte(`{"command":"\u0072m -rf /tmp"}`)
	r := reFor(t, `\brm\b.*-[a-z]*r[a-z]*f`)

	if ruleMatches(r, "Bash", raw, "") {
		t.Fatal("原始字节不应直接命中（它确实不含字符 r）")
	}
	norm := NormalizeToolInput(raw)
	if norm == "" {
		t.Fatal("合法 JSON 应产出规范化文本")
	}
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("解码后应命中，规范化结果=%q", norm)
	}
}

func TestNormalizeNonJSONPassthrough(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(""), []byte("not json"), []byte(`["a"]`)} {
		if got := NormalizeToolInput(in); got != "" {
			t.Fatalf("输入 %q 应返回空串，得到 %q", in, got)
		}
	}
}

func TestNormalizeDoesNotMaskRawMatch(t *testing.T) {
	// 正常明文输入两种形态都应命中，保证既有规则行为不变（零行为差异）。
	r := reFor(t, `rm\s+-rf`)
	raw := []byte(`{"command":"rm -rf /tmp"}`)
	if !ruleMatches(r, "Bash", raw, NormalizeToolInput(raw)) {
		t.Fatal("明文输入应保持命中")
	}
}

func TestNormalizeUnblocksDoubleEscapedBypass(t *testing.T) {
	// 双重转义：传输层解码后 payload 仍是字面 \u0072m——一层 Unmarshal 不够，
	// 递归解码必须解到真实命令词。raw 即 {"command":"\\u0072m -rf /tmp"}：
	// 一层 Unmarshal 解出字面 `\u0072m`，递归后才是 rm。
	raw := []byte(`{"command":"\\\\u0072m -rf /tmp"}`)
	r := reFor(t, `\brm\b.*-[a-z]*r[a-z]*f`)

	norm := NormalizeToolInput(raw)
	if strings.Contains(norm, `\u0072`) {
		t.Fatalf("递归解码不应残留字面转义形态：%q", norm)
	}
	if !strings.Contains(norm, "rm -rf") {
		t.Fatalf("应解出真实命令词：%q", norm)
	}
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("双重转义 payload 解码后应命中：%q", norm)
	}
}

func TestDecodeEscapedStringStopsOnIllegalForms(t *testing.T) {
	// 非法/无变化形态保持原样（fail-safe：少解；残余形态有 raw 原文兜底匹配）。
	for _, in := range []string{
		"plain command",         // 无反斜杠：直接返回
		`literal \x41 not json`, // 非法转义：Unmarshal 失败保持原样
		"line\nbreak",           // 字面换行（Go 字符串真实换行）：包引号 Unmarshal 失败保持原样
	} {
		if got := decodeEscapedString(in); got != in {
			t.Fatalf("非法形态应保持原样：%q → %q", in, got)
		}
	}
	// 合法多层：三层转义解到明文 curl。
	deep := `\\\\u0063\\\\u0075\\\\u0072\\\\u006c`
	if got := decodeEscapedString(deep); got != "curl" {
		t.Fatalf("多层转义应解到明文：got %q", got)
	}
}

func TestDecodeValueWalksNestedStructures(t *testing.T) {
	// args 数组里嵌套的转义命令串同样要被递归解开。meta 不在归一化字段白名单
	// 内（fields），本就不参与匹配——断言只针对白名单字段。
	input := []byte(`{"args":["\\\\u0063url http://x","ok"]}`)
	norm := NormalizeToolInput(input)
	if !strings.Contains(norm, "curl http://x") {
		t.Fatalf("嵌套结构内的转义应被递归解开：%q", norm)
	}
}
