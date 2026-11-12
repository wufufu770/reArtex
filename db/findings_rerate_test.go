package db

import "testing"

// E1-7 存量重评规则（确定性，无库）：
// ① 乱值 severity → info；② info_disclosure 且无凭据链特征 → info；
// ③ info_disclosure 但证据带凭据特征 → 不出候选（留人工）。
func TestReratePropose(t *testing.T) {
	if p, r := reratePropose("sqli", "严重", "x", "y"); p != SeverityInfo || r == "" {
		t.Fatalf("乱值 severity 应出候选 → info, got %q/%q", p, r)
	}
	if p, _ := reratePropose("info_disclosure", "low", "目录列表可读", "GET /backup/ 返回索引页"); p != SeverityInfo {
		t.Fatalf("纯信息泄露无凭据特征应出候选 → info, got %q", p)
	}
	if p, _ := reratePropose("info_disclosure", "medium", "可读数据库密码文件", "明文 password=root"); p != "" {
		t.Fatalf("证据带凭据特征不应出候选（留人工）, got %q", p)
	}
	if p, _ := reratePropose("sqli", "high", "union 注入读库", "有 PoC"); p != "" {
		t.Fatalf("正常高危及非 info_disclosure 不应被动, got %q", p)
	}
	if p, _ := reratePropose("info_disclosure", "info", "x", "y"); p != "" {
		t.Fatalf("已是 info 不应再出候选, got %q", p)
	}
}
