package intercept

import (
	"regexp"
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
	norm := normalizeToolInput(raw)
	if norm == "" {
		t.Fatal("合法 JSON 应产出规范化文本")
	}
	if !ruleMatches(r, "Bash", raw, norm) {
		t.Fatalf("解码后应命中，规范化结果=%q", norm)
	}
}

func TestNormalizeNonJSONPassthrough(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(""), []byte("not json"), []byte(`["a"]`)} {
		if got := normalizeToolInput(in); got != "" {
			t.Fatalf("输入 %q 应返回空串，得到 %q", in, got)
		}
	}
}

func TestNormalizeDoesNotMaskRawMatch(t *testing.T) {
	// 正常明文输入两种形态都应命中，保证既有规则行为不变（零行为差异）。
	r := reFor(t, `rm\s+-rf`)
	raw := []byte(`{"command":"rm -rf /tmp"}`)
	if !ruleMatches(r, "Bash", raw, normalizeToolInput(raw)) {
		t.Fatal("明文输入应保持命中")
	}
}
