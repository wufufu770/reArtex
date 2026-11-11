package db

import (
	"encoding/json"
	"strings"
)

// ---------- 意图种类（T5，方案 v10 §6 三期）----------
//
// 意图 payload 的可选 kind 字段：按种类限并发（dispatch_kind_caps）的数据落点。
// 词表用 reArtex 自己的探索动词（侦察/利用/验证），自由文本归一到这四个词——
// 模型写「信息收集」「踩点」归 recon，写「漏洞验证」「复测」归 verify，认不出
// 的归 general。归一放在【写入时】（add_intent 落 payload 前），认领侧只做等值
// 比较，不重复归一。
//
// 词表是代码常量而不是 settings：容量闸（dispatch_kind_caps）的键匹配依赖归一
// 口径稳定；词表漂移会让已写入的 kind 在认领侧对不上号。

// 意图种类的规范词（general 是兜底——payload 无 kind 字段也归它；容量闸只按
// 显式种类设限，general 不参与）。
const (
	IntentKindRecon   = "recon"   // 侦察/收集：被动情报、字典枚举、端口扫描、内容提取
	IntentKindExploit = "exploit" // 利用/攻击：注入、爆破、payload、武器化
	IntentKindVerify  = "verify"  // 验证/复测：证明/证伪一个具体结论
	IntentKindGeneral = "general" // 未标注/认不出
)

// intentKindAliases 是规范词 → 自由文本别名的判定词表（小写包含匹配）。
// 刻意不收过宽的词（如「资产」——「对资产X做注入测试」会被误归 recon）：
// 误分类的代价是种类容量闸限错对象，宁可漏归 general。
var intentKindAliases = map[string][]string{
	IntentKindRecon:   {"收集", "侦察", "踩点", "情报", "枚举", "指纹", "recon", "enumerate", "fingerprint", "discovery", "scan"},
	IntentKindExploit: {"利用", "攻击", "渗透", "爆破", "注入", "武器化", "getshell", "拿shell", "exploit", "brute", "inject", "attack", "pwn"},
	IntentKindVerify:  {"验证", "复测", "核实", "复核", "证明", "verify", "retest", "confirm", "prove", "validate"},
}

// NormalizeIntentKind 把模型给的自由文本 kind 归一到规范词表。
// 空串/认不出 → general。匹配顺序 recon → exploit → verify（判定词表刻意
// 不重叠；一个词同时命中多类时取先命中者）。
func NormalizeIntentKind(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return IntentKindGeneral
	}
	for _, kind := range []string{IntentKindRecon, IntentKindExploit, IntentKindVerify} {
		for _, alias := range intentKindAliases[kind] {
			if strings.Contains(s, alias) {
				return kind
			}
		}
	}
	return IntentKindGeneral
}

// IntentKindFromPayload 读意图 payload 里的 kind 字段（自由文本归一）；
// 无字段/空/解析失败 → general。
func IntentKindFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return IntentKindGeneral
	}
	var p struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return IntentKindGeneral
	}
	return NormalizeIntentKind(p.Kind)
}
