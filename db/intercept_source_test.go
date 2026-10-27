package db

import "testing"

// 拦截记录的来源标签必须能区分三类：DB 规则、模型兜底审批、代码常量短路表。
// 短路表那一类靠 audit 里的 RuleName 前缀识别 —— 前缀是 guard/shortcircuit.go
// 与本包之间的约定，改了会静默失效（标签落回 unknown 而不报错）。
func TestInterceptSource(t *testing.T) {
	cases := []struct {
		desc    string
		ruleID  int64
		reason  string
		audits  []*InterceptAudit
		want    string
	}{
		{
			desc:   "DB 规则命中",
			ruleID: 7,
			reason: "危险命令",
			want:   "rule",
		},
		{
			desc:   "模型兜底审批",
			ruleID: 0,
			reason: "[模型] 该操作需要人工确认",
			want:   "model",
		},
		{
			desc:   "代码常量短路表",
			ruleID: 0,
			reason: "平台凭据/进程环境",
			audits: []*InterceptAudit{{RuleName: "[代码常量] 平台凭据/进程环境"}},
			want:   "shortcircuit",
		},
		{
			desc:   "无来源可判",
			ruleID: 0,
			reason: "某种没标记来源的判定",
			want:   "unknown",
		},
		{
			desc:   "audit 为 nil 时不 panic",
			ruleID: 0,
			reason: "无来源",
			audits: []*InterceptAudit{nil},
			want:   "unknown",
		},
	}
	for _, c := range cases {
		if got := interceptSource(c.ruleID, c.reason, c.audits); got != c.want {
			t.Fatalf("%s：got %q want %q", c.desc, got, c.want)
		}
	}
}
