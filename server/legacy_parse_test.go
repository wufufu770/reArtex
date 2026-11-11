package server

// v0.10.1 回归：蒸馏解析容错。LLM 产出的 JSON 数组常带非法字符（字符串值里
// 的裸换行、尾逗号），此前 json.Unmarshal 一失败就把整坨数组文本塞进单条经验
// 的 summary（task#9 现场：标题=任务目标、正文=```json 围栏全文）——既不通用
// 也不可读。修好+逐对象打捞后必须拆出多条结构化经验。

import (
	"strings"
	"testing"
)

func TestParseLegacyItemsTolerant(t *testing.T) {
	// 形态 1：字符串值里有裸换行（JSON 非法，LLM 高频产出）。
	brokenNewline := "```json\n[\n  {\n    \"kind\": \"experience\",\n    \"title\": \"VSB9 站群同因漏洞可跨 vhost 批量复用\",\n    \"summary\": \"第一行结论。\n第二行验证方式（裸换行）。\",\n    \"fingerprint\": {\"目标类型\": \"web\"}\n  },\n  {\n    \"kind\": \"pitfall\",\n    \"title\": \"注入复核当 oracle 退化时不要下安全结论\",\n    \"summary\": \"先重建非零 oracle 再试载荷。\n通道受阻时写受阻未穷尽。\"\n  }\n]\n```"
	items := parseLegacyItems(brokenNewline)
	if len(items) != 2 {
		t.Fatalf("裸换行应修复并拆出 2 条，拿到 %d 条", len(items))
	}
	if items[0].Kind != "experience" || items[1].Kind != "pitfall" {
		t.Fatalf("拆出的条目 kind 不对: %q / %q", items[0].Kind, items[1].Kind)
	}
	if !strings.Contains(items[0].Summary, "第二行验证方式") {
		t.Fatalf("换行内容丢失: %q", items[0].Summary)
	}

	// 形态 2：尾逗号。
	brokenTrailing := `[{"kind":"experience","title":"t1","summary":"s1",},{"kind":"pitfall","title":"t2","summary":"s2",},]`
	items = parseLegacyItems(brokenTrailing)
	if len(items) != 2 {
		t.Fatalf("尾逗号应修复并拆出 2 条，拿到 %d 条", len(items))
	}

	// 形态 3：第一个对象彻底坏（title 缺引号），第二个完好——打捞应保住好的那个。
	mixed := `[{"kind":"experience","title": 坏掉的值,"summary":"x"}, {"kind":"fingerprint_summary","title":"画像","summary":"32 子域 23 存活"}]`
	items = parseLegacyItems(mixed)
	if len(items) != 1 || items[0].Kind != "fingerprint_summary" {
		t.Fatalf("逐对象打捞应保住完好的 1 条，拿到 %+v", items)
	}

	// 形态 4：合法 JSON 不受影响（直通路径）。
	good := `[{"kind":"experience","title":"a","summary":"b"}]`
	if items = parseLegacyItems(good); len(items) != 1 {
		t.Fatalf("合法 JSON 应直通，拿到 %d 条", len(items))
	}

	// 形态 5：纯杂文（非 JSON）——保持原语义：解析失败返回 nil，由调用方走
	// 单条全量 fallback。
	if items := parseLegacyItems("模型输出了一段没有数组的散文"); items != nil {
		t.Fatalf("纯杂文应返回 nil 走 fallback，拿到 %+v", items)
	}
}
