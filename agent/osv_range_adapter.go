// osv_range_adapter.go：OSV affected ranges → B-05 三态求值适配（B-08C）。
//
// 唯一目标：把 B-08B 拿到的 OSV affected ranges（introduced/fixed/
// last_affected 事件形态）转成 B-03D ranges 契约，经 B-01D 只读客户端的
// EvaluateRanges（cve_match_component 纯求值模式）由 CVE 服务内部的 B-05
// version.VersionInRange 求值 affected/not_affected/maybe。
//
// 判定纪律（包安全原则逐字）：
//   - 单体绝不复制版本比较器：本文件零区间判定逻辑，只做形态转换与三态
//     透传（绝不 import reartex-cve-mcp/version——独立 module 边界）；
//   - B-05 三态保持原命名：affected / not_affected / maybe（db.RangeVerdict
//     同值对齐）；任何无法确定的结果一律 maybe；
//   - 不能把「查不到」「网络失败」「版本无法比较」解释成 not_affected：
//     求值失败/区间不可用/名字或生态不匹配/缺版本 → maybe 带如实 reason；
//   - not_affected 只能来自 B-05 的透传结果——适配器自身绝不产生
//     not_affected（无命中绝不误判为不受影响）；
//   - OSV 结果只是情报线索：本文件零 finding 写入、零任务创建、零 scope
//     变更（持久化与去重属 B-08D，触发接线属 B-08E）。
//
// 转换纪律：OSV 事件绝不静默丢弃——一条 range 内出现未知事件（如 limit）、
// 空版本或畸形事件 ⇒ 整条 range 记 skipped 并如实计数（丢事件等于静默改
// 数据）；仅 SEMVER/ECOSYSTEM 区间可求值，GIT/未知类型记 skipped。仅显式
// versions 清单可做精确等值命中（affected）——绝不从版本串猜测区间语义。
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// osvMaxEvalRanges 与侧车 B-03D adapterMaxCallerRanges=32 同值的前置闸：
// 超上限直接 maybe（绝不截断后半判——截断区间等于静默改判定材料）。
const osvMaxEvalRanges = 32

// osvMaxRangeEvents 与侧车每条 events 上限 16 同值的前置闸：超上限整条
// range 不可求值（记 skipped），不截断。
const osvMaxRangeEvents = 16

// OSVVulnVerdict 是单条漏洞对单条依赖的三态判定（保留 OSV ID/CVE alias 与
// reason——绝不只保留布尔值，卡要求 5）。
type OSVVulnVerdict struct {
	VulnID  string          // OSV 漏洞 ID（GHSA/…）
	Aliases []string        // CVE 等 alias 原样保留
	Verdict db.RangeVerdict // affected / not_affected / maybe（B-05 命名）
	Reason  string          // 判定依据（B-05 reason 透传 + 适配层如实补充）
}

// OSVDepVerdict 是单条依赖的全部漏洞判定汇总。
type OSVDepVerdict struct {
	Ref           OSVQueryRef
	Vulns         []OSVVulnVerdict
	SkippedNonNPM int // B-08B 过滤计数原样透传（非 npm affected 已滤除的事实）
}

// EvaluateOSVHits 批量求值：逐依赖逐漏洞调 B-05（每次 EvaluateRanges 一条
// 漏洞——次数与漏洞总数线性有界）。hits 为空=合法空结果非 nil。
func EvaluateOSVHits(ctx context.Context, eval db.VersionRangeEvaluator, hits []OSVQueryHits) []OSVDepVerdict {
	out := make([]OSVDepVerdict, 0, len(hits))
	for _, h := range hits {
		vulns := make([]OSVVulnVerdict, 0, len(h.Vulns))
		for _, v := range h.Vulns {
			vulns = append(vulns, EvaluateOSVVuln(ctx, eval, h.Ref, v))
		}
		out = append(out, OSVDepVerdict{Ref: h.Ref, Vulns: vulns, SkippedNonNPM: h.SkippedNonNPM})
	}
	return out
}

// EvaluateOSVVuln 对单条依赖×单条漏洞求值三态。所有无法确定路径一律 maybe
// 带如实 reason；eval 为 nil 时 maybe（无求值器不可判）。
func EvaluateOSVVuln(ctx context.Context, eval db.VersionRangeEvaluator, ref OSVQueryRef, vuln OSVVuln) OSVVulnVerdict {
	out := OSVVulnVerdict{
		VulnID:  strings.TrimSpace(vuln.ID),
		Aliases: vuln.Aliases,
		Verdict: db.VerdictMaybe,
	}
	if out.VulnID == "" {
		out.Reason = "漏洞缺 ID（不可信响应防御）——判 maybe"
		return out
	}
	if eval == nil {
		out.Reason = "无求值器——判 maybe"
		return out
	}
	ver := strings.TrimSpace(ref.Version)
	if ver == "" {
		out.Reason = "依赖缺版本——无法求值，判 maybe"
		return out
	}

	// ① 匹配本依赖的 affected 条目（生态 npm + 名字一致，大小写不敏感——
	// OSV 生态有 "NPM" 写法；不匹配条目如实计数）。
	matched, skipped := 0, 0
	for i := range vuln.Affected {
		a := vuln.Affected[i]
		if strings.EqualFold(strings.TrimSpace(a.Package.Ecosystem), osvEcosystemNPMQuery) &&
			strings.EqualFold(strings.TrimSpace(a.Package.Name), strings.TrimSpace(ref.Name)) {
			matched++
		} else {
			skipped++
		}
	}
	if matched == 0 {
		// 无命中绝不判 not_affected（卡要求 4 逐字）。
		out.Reason = fmt.Sprintf("OSV affected 与依赖（生态/名字）不匹配（跳过 %d 条）——判 maybe", skipped)
		return out
	}

	// ② 显式 versions 清单精确等值命中 ⇒ affected（OSV 自身的权威声明；
	// 只做字面等值，绝不做区间语义猜测）。等值未命中⇒继续走区间求值，
	// 绝不据此判 not_affected。
	for i := range vuln.Affected {
		a := vuln.Affected[i]
		if !osvAffectedMatchesRef(a, ref) {
			continue
		}
		for _, ev := range a.Versions {
			if strings.TrimSpace(ev) == ver {
				out.Verdict = db.VerdictAffected
				out.Reason = "命中 OSV 显式版本清单（精确等值）"
				return out
			}
		}
	}

	// ③ 区间转换：仅 SEMVER/ECOSYSTEM 可求值；事件形态逐条转换（单键
	// introduced/fixed/last_affected → 版本），任何未知事件/空版本/畸形
	// ⇒ 整条 range 记 skipped（绝不丢事件半转换）。
	ranges := make([]cveintel.Range, 0, len(vuln.Affected))
	unusable := 0
	for i := range vuln.Affected {
		a := vuln.Affected[i]
		if !osvAffectedMatchesRef(a, ref) {
			continue
		}
		for _, r := range a.Ranges {
			cr, ok := osvRangeToCveintel(r)
			if !ok {
				unusable++
				continue
			}
			ranges = append(ranges, cr)
		}
	}
	if unusable > 0 && len(ranges) == 0 {
		out.Reason = fmt.Sprintf("无可用区间（%d 条不可转换：GIT/未知类型/未知事件/空版本/空区间）——判 maybe", unusable)
		return out
	}
	if len(ranges) == 0 {
		out.Reason = "漏洞无区间信息——判 maybe（绝不判 not_affected）"
		return out
	}
	if len(ranges) > osvMaxEvalRanges {
		out.Reason = fmt.Sprintf("可求值区间 %d 条超上限 %d（绝不截断后半判）——判 maybe", len(ranges), osvMaxEvalRanges)
		return out
	}

	// ④ 经 B-01D 客户端调 B-03D 纯求值（B-05 唯一判定点）。失败 ⇒ maybe
	// ——网络/服务失败绝不当「无漏洞」。
	verdict, reason, err := eval.EvaluateRanges(ctx, osvEcosystemNPMQuery, ver, ranges)
	if err != nil {
		out.Reason = fmt.Sprintf("区间求值失败（%v）——判 maybe（失败绝不当无漏洞）", err)
		return out
	}
	switch db.RangeVerdict(strings.TrimSpace(verdict)) {
	case db.VerdictAffected, db.VerdictNotAffected, db.VerdictMaybe:
		out.Verdict = db.RangeVerdict(strings.TrimSpace(verdict))
	default:
		// 未知三态值=不可信响应（cveintel 面已闸，纵深防御）。
		out.Reason = fmt.Sprintf("求值返回未知三态 %q——判 maybe", verdict)
		return out
	}
	out.Reason = reason
	if skipped > 0 || unusable > 0 {
		out.Reason = fmt.Sprintf("%s（另有 %d 条 affected 不匹配、%d 条区间不可转换）", out.Reason, skipped, unusable)
	}
	return out
}

// osvAffectedMatchesRef 判断 affected 条目是否属于本依赖（生态 npm+名字一致）。
func osvAffectedMatchesRef(a OSVAffected, ref OSVQueryRef) bool {
	return strings.EqualFold(strings.TrimSpace(a.Package.Ecosystem), osvEcosystemNPMQuery) &&
		strings.EqualFold(strings.TrimSpace(a.Package.Name), strings.TrimSpace(ref.Name))
}

// osvRangeToCveintel 把一条 OSV range 转成 B-03D events 形态；不可求值
// （GIT/未知类型、空区间、未知事件、空版本、超每条事件上限）返回 false。
func osvRangeToCveintel(r OSVRange) (cveintel.Range, bool) {
	switch strings.ToUpper(strings.TrimSpace(r.Type)) {
	case "SEMVER", "ECOSYSTEM":
	default:
		return cveintel.Range{}, false
	}
	if len(r.Events) == 0 {
		return cveintel.Range{}, false // 空区间
	}
	if len(r.Events) > osvMaxRangeEvents {
		return cveintel.Range{}, false // 超每条上限：整条不可求值，不截断
	}
	events := make([]cveintel.RangeEvent, 0, len(r.Events))
	for _, e := range r.Events {
		pairs := 0
		if v := strings.TrimSpace(e.Introduced); v != "" {
			events = append(events, cveintel.RangeEvent{"introduced": v})
			pairs++
		}
		if v := strings.TrimSpace(e.Fixed); v != "" {
			events = append(events, cveintel.RangeEvent{"fixed": v})
			pairs++
		}
		if v := strings.TrimSpace(e.LastAffected); v != "" {
			events = append(events, cveintel.RangeEvent{"last_affected": v})
			pairs++
		}
		// limit 或未知扩展事件：OSVRangeEvent 未建模的形态无法确认——为不
		// 静默丢事件，凡 presence 即整条不可求值（比引入第四种键更保守）。
		if strings.TrimSpace(e.Limit) != "" {
			return cveintel.Range{}, false
		}
		if pairs == 0 {
			return cveintel.Range{}, false // 空事件对象=未知形态
		}
	}
	return cveintel.Range{Ecosystem: osvEcosystemNPMQuery, Events: events}, true
}
