package agent

import (
	"errors"
	"regexp"
	"strings"
)

// 本文件是 finding 落库前的确定性启发式预检（XIB-Artex finding_gate.go 的本仓化）。
//
// 为什么要这一层：worker 纪律（report_finding 提示词）已写明「没有 PoC 不报、存疑用
// record_fact」，但模型违规时，违规上报会先落库成 pending、再走人工/复核打回——「提出」
// 占了复核带宽，「提出即 pending」还占着探索图节点。预检把三类【注定被复核打回】的
// 上报在工具层当场拒绝（与 severity 归一化同一形态：当场拒 + 给改写方向），让模型
// 立即改道 record_fact，复核层只看真正过了预检的候选。
//
// 铁律对齐：「模型只提议，确定性代码裁决」——预检是确定性规则，不调 LLM、不进 DB/
// settings 热改面，词表/正则挂在源码常量上（与 guard/shortcircuit.go 同一纪律）。
//
// 刻意不移植的两条（XIB 原版有，本仓裁决拒绝，理由留档）：
//   - 缺 HTTP 交互标记即拒：reArtex 明文支持 TCP 等非 HTTP 漏洞省略流量绑定（提示词
//     与 traffic_refs 规约两处写死），这条会误伤合法 TCP 类上报；
//   - "decompiled" 字面拦截：小程序渗透的合法工作流包含 wxapkg 反编译产物中的
//     硬编码凭据泄露上报（key 泄露本身是洞），字面拦会误伤 miniprogram 场景。
//
// 规则面刻意保守，只拦【模型自己承认】证据不足或结论自否的上报：
//   R1 evidence 空白 —— 证据是 finding 的存在前提，XIB 原版同款；
//   R2 summary 自否 —— 结论陈述里出现「不是漏洞/误报/不成立」= 模型自己否定了自己；
//   R3 summary 自认未验证 —— 「未验证/未触发/无 PoC/待验证/存疑」出现在【结论陈述】里
//     = 自认不满足「没有 PoC 不报」纪律。evidence 不参与 R2/R3：证据文本里引用上下文
//     （复测记录、目标回复「此漏洞不存在」等）出现这些词是正常的。

var (
	// findingGateSelfRefutingRe 匹配 summary 里的自否与自认未验证表述（R2+R3）。
	findingGateSelfRefutingRe = regexp.MustCompile(
		`(?i)(不是(一个|个)?漏洞|非漏洞|不构成漏洞|不成立|未成立|误报|false\s*positive|not\s*a\s*vuln|false\s*alarm` +
			`|无法复现|未能复现|未复现|未触发|未能触发|未验证|未经验证|未取得\s*poc|无\s*poc|没有\s*poc` +
			`|待验证|存疑)`)
)

// CheckFindingGate 是 report_finding 落库前的确定性预检。nil = 放行。
//
// 只收 summary 与 evidence 两个判定面，刻意不做范围/种类判定（那是 auditor_gate
// 与 anchorAssetsInScope 的职责，不重复设闸）。
func CheckFindingGate(summary, evidence string) error {
	// R1：证据空白。Finding 的存在前提是「有可验证证据」，没有就不该进复核带宽。
	if strings.TrimSpace(evidence) == "" {
		return errors.New("证据为空")
	}
	// R2+R3：结论自否 / 自认未验证。summary 是模型的结论陈述——它自己都说
	// 「不是漏洞」「未验证」，这条上报注定被复核打回，当场改道 record_fact。
	if findingGateSelfRefutingRe.MatchString(summary) {
		return errors.New("结论自否/自认未验证（summary 含自否或未验证表述）")
	}
	return nil
}
