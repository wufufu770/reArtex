package server

// 方案 v3 缺陷 D1：蒸馏「无产出」判定的单测。
//
// 模型被明确告知「没有值得传承的就输出 []」。旧实现把字面 "[]" 当 summary
// 存成脏节点（传承页满屏 `[]`）。本组用例钉住：空数组（裸/带围栏/杂文包裹）
// 一律判为「无产出」，纯杂文（解析不出数组）仍走单条全量经验 fallback。

import "testing"

func TestLegacyEmptyOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"裸空数组", "[]", true},
		{"带空白", "  [ ]  ", true},
		{"json 围栏", "```json\n[]\n```", true},
		{"裸围栏", "```\n[]\n```", true},
		{"杂文包裹", "本次任务没有值得沉淀的经验：[]", true},
		{"杂文包裹带说明", "结论如下\n\n[]\n\n（无）", true},
		{"非空数组", `[{"kind":"experience","title":"t","summary":"s"}]`, false},
		{"纯杂文", "这次任务收获很大， conclusion：目标达标。", false},
		{"空串", "", false},
		{"只有围栏", "```json\n```", false},
	}
	for _, tc := range cases {
		if got := legacyEmptyOutput(tc.in); got != tc.want {
			t.Errorf("%s: legacyEmptyOutput(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestParseLegacyItemsEmptyArray(t *testing.T) {
	// 空数组 parse 出 0 条 + legacyEmptyOutput 判真 ⇒ 两条一起构成「无产出」；
	// 纯杂文 parse 出 0 条但判假 ⇒ 走 fallback。钉住这两条的分界。
	if items := parseLegacyItems("[]"); len(items) != 0 {
		t.Fatalf("空数组应解析出 0 条，got %d", len(items))
	}
	if items := parseLegacyItems("纯杂文，不是 JSON"); len(items) != 0 {
		t.Fatalf("杂文应解析出 0 条，got %d", len(items))
	}
	if !legacyEmptyOutput("[]") || legacyEmptyOutput("纯杂文，不是 JSON") {
		t.Fatal("空产出判定与解析结果必须互补：[] 判真、杂文判假")
	}
}
