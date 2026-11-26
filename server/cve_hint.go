package server

// CVE 情报 hint 发出器（B-04F）：把反向匹配（db.MatchCVEToAssets）的命中经
// 【现有 hint 写路径】挂到引用资产的未终态任务上。纪律逐条对应原卡：
//   - 命中只是 hint：绝不直接生成 finding、不授权利用、不扩大任务 scope——
//     hint 文本固定模板最后一句就是「请验证后决定是否立项验证意图」；
//   - 走现有工具链写路径：节点写入走 AddNodeInScope（agent 可达、锚点先判
//     范围的唯一 hint 写入口，add_hint 工具的 addOneHint 同款）——资产不在
//     任务范围内整单拒绝（范围即授权边，越界绝不写）。宿主批次发出器与
//     agent/bg_task.go 的 "background_task" 同语义（系统批量写者，origin
//     可溯源、不参与读取过滤），SoD/审批包装是 agent 会话内的工具调用层，
//     批次发出器不经会话——如实申报为宿主可信路径 + 最强授权边（范围判定）。
//   - 去重：先查 cve_hints 台账（B-04E HasCVEHint 快速跳过，重跑同步零重复），
//     发出成功后 RecordCVEHint 落账（唯一约束是终闸）；
//   - 节流：单批对同一任务最多 cveHintMaxPerTaskPerBatch 条（卡默认 20），
//     超额记日志跳过——台账无行，下批重试（记日志不丢库）；
//   - 不唤醒 planner：hint 在任务下轮自然可见（与 add_hint 工具的 notifyHint
//     语义不同——批次发出绝不注入运行时中断）。

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// cveHintMaxPerTaskPerBatch 单批同步对同一任务的 hint 上限（原卡默认 20）。
const cveHintMaxPerTaskPerBatch = 20

// cveMatchBatchMax 单批枚举 CVE 上限（sidecar search 的 limit 硬顶 100）；
// cveMatchTokensMax 单 CVE 产品 token 上限（token 风暴防线：一个 CVE 的区间包名
// 可能一大把，取规范化后的前 8 个，顺序＝区间原始顺序）。
const (
	cveMatchBatchMax  = 100
	cveMatchTokensMax = 8
)

// CVEHintEmitStats 是一次批量发出的统计（调用方记日志用）。
type CVEHintEmitStats struct {
	Considered int // 审阅的 (匹配, 任务) 对数
	Emitted    int // 实际发出并落账
	Deduped    int // 台账命中跳过（重跑收敛）
	Throttled  int // 超单任务单批上限跳过（下批重试）
	Skipped    int // 任务缺失/范围拒绝等逐项跳过
}

// cveHintCVSS 渲染 CVSS 分（v3 优先，v4 兜底，都无则「无」）。
func cveHintCVSS(rec *cveintel.Record) string {
	switch {
	case rec.CVSSv3 != nil:
		return fmt.Sprintf("%.1f", *rec.CVSSv3)
	case rec.CVSSv4 != nil:
		return fmt.Sprintf("%.1f", *rec.CVSSv4)
	default:
		return "无"
	}
}

// RenderCVEHint 固定模板（原卡明文）：「CVE 情报：资产 <X> 疑似受 <CVE> 影响
// （<判定>），CVSS <分>，KEV=<是/否>，请验证后决定是否立项验证意图」。判定保留
// B-05 三态原名（affected/maybe），并附命中明细（资产版本/缺失原因）供规划者
// 自行判断置信度——「疑似」语义绝不允许被渲染成「已确认」。
func RenderCVEHint(m db.AssetMatch, rec *cveintel.Record) string {
	detail := ""
	switch {
	case m.MatchedVersion != "" && m.Verdict == db.VerdictAffected:
		detail = fmt.Sprintf("，资产版本 %s 命中影响区间", m.MatchedVersion)
	case m.MatchedVersion != "":
		detail = fmt.Sprintf("，资产版本 %s，%s", m.MatchedVersion, strings.TrimSpace(m.Reason))
	default:
		detail = "，资产指纹无版本号"
	}
	return fmt.Sprintf("CVE 情报：资产 %s 疑似受 %s 影响（%s%s），CVSS %s，KEV=%s，请验证后决定是否立项验证意图",
		m.AssetDomain, rec.CVEID, m.Verdict, detail, cveHintCVSS(rec), map[bool]string{true: "是", false: "否"}[rec.InKEV])
}

// EmitCVEHints 把一批匹配结果对每个（资产，未终态任务）对发出至多一条 hint。
// 确定性：任务按升序处理（B-04D 归属查询已排序，这里再排序防御）；台账失败
// 整批报错（hint 节点已写而台账未落 = 下批可能重发，必须暴露而非吞掉）。
func (s *Server) EmitCVEHints(ctx context.Context, matches []db.AssetMatch, rec *cveintel.Record) (CVEHintEmitStats, error) {
	var stats CVEHintEmitStats
	if s.m == nil || s.m.pg == nil {
		return stats, fmt.Errorf("cve_hint: 数据库未就绪")
	}
	if rec == nil || strings.TrimSpace(rec.CVEID) == "" {
		return stats, fmt.Errorf("cve_hint: CVE 情报记录必填（cve_id 非空）")
	}
	pg := s.m.pg
	perTask := map[int64]int{}
	for _, m := range matches {
		// 防御：not_affected 在匹配层已剔除，绝不允许经发出器复活。
		if m.Verdict != db.VerdictAffected && m.Verdict != db.VerdictMaybe {
			return stats, fmt.Errorf("cve_hint: 匹配 %s 携带非法判定 %q（只收 affected/maybe）", rec.CVEID, m.Verdict)
		}
		taskIDs := append([]int64(nil), m.TaskIDs...)
		sort.Slice(taskIDs, func(i, j int) bool { return taskIDs[i] < taskIDs[j] })
		for _, taskID := range taskIDs {
			stats.Considered++
			if perTask[taskID] >= cveHintMaxPerTaskPerBatch {
				stats.Throttled++ // 台账无行：下批重试（记日志不丢库）
				log.Printf("[cve-hint] 任务 %d 本批 hint 达上限 %d，CVE %s 资产 #%d 留待下批",
					taskID, cveHintMaxPerTaskPerBatch, rec.CVEID, m.AssetID)
				continue
			}
			exists, err := pg.HasCVEHint(taskID, m.AssetID, rec.CVEID)
			if err != nil {
				return stats, fmt.Errorf("cve_hint: 去重查询失败（任务 %d 资产 #%d）: %w", taskID, m.AssetID, err)
			}
			if exists {
				stats.Deduped++
				continue
			}
			task, err := pg.GetTask(taskID)
			if err != nil || task == nil || task.ExplorationID <= 0 {
				stats.Skipped++
				log.Printf("[cve-hint] 任务 %d 不可达（已删除或无探索图），CVE %s 资产 #%d 跳过: %v",
					taskID, rec.CVEID, m.AssetID, err)
				continue
			}
			text := RenderCVEHint(m, rec)
			payload := map[string]any{
				"text":     text,
				"cve_id":   rec.CVEID,
				"verdict":  string(m.Verdict),
				"asset_id": m.AssetID,
			}
			if m.MatchedVersion != "" {
				payload["matched_version"] = m.MatchedVersion
			}
			// 现有 hint 写路径：AddNodeInScope（add_hint 工具 addOneHint 同款）——
			// 锚点先判范围，资产不在任务范围内整单拒绝（逐项跳过不中断整批）。
			nodeID, err := pg.Exploration(task.ExplorationID).AddNodeInScope(
				taskID, db.KindHint, payload, 0, "active", "cve-intel", []int64{m.AssetID})
			if err != nil {
				stats.Skipped++
				log.Printf("[cve-hint] 任务 %d 写 hint 被拒（CVE %s 资产 #%d，多为范围判定）: %v",
					taskID, rec.CVEID, m.AssetID, err)
				continue
			}
			inserted, err := pg.RecordCVEHint(taskID, m.AssetID, rec.CVEID, string(m.Verdict), nodeID)
			if err != nil {
				return stats, fmt.Errorf("cve_hint: 台账落账失败（任务 %d 资产 #%d 节点 #%d）——hint 节点已写而台账未落，下批可能重发，必须暴露: %w",
					taskID, m.AssetID, nodeID, err)
			}
			_ = inserted // 并发批次收敛时 inserted=false：hint 已存在，不再计数
			perTask[taskID]++
			stats.Emitted++
		}
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// B-04G：新 CVE 批次触发匹配（同步批次 → 匹配器 → hint 发出的调度原语）
// ---------------------------------------------------------------------------

// CVEBatchStats 是一次批次的汇总（调用方记日志）。
type CVEBatchStats struct {
	Swept         int  // 枚举到的 CVE 数
	NoTokens      int  // 规范化后无产品 token 跳过（含糊标题/无区间包名——绝不猜）
	MatchedAssets int  // 匹配出（受影响/可能受影响）的资产次
	Emitted       int  // 发出的 hint 数
	Deduped       int  // 台账去重数
	Throttled     int  // 单任务节流数
	Skipped       int  // 任务缺失/范围拒绝等跳过数
	Unavailable   bool // CVE_MCP_URL 未配置（B-01E 静默降级，调用方记一次日志）
}

// cveTokensFromRecord 从 CVE 情报记录提取产品 token：①区间包名（结构化优先，
// 逐个过 NormalizeTechToken + IsValidTechTokenQuery 合同过滤）；②区间零产出时
// 回落标题（单 token 尝试）。去重保序（区间原始顺序），封顶 cveMatchTokensMax。
// 含糊包名/标题被规范化层拒绝（OK=false）——绝不猜产品。
func cveTokensFromRecord(rec *cveintel.Record) []string {
	tokens := make([]string, 0, cveMatchTokensMax)
	seen := make(map[string]bool, cveMatchTokensMax)
	add := func(raw string) {
		if len(tokens) >= cveMatchTokensMax {
			return
		}
		tt := db.NormalizeTechToken(raw)
		if !tt.OK || !db.IsValidTechTokenQuery(tt.Product) || seen[tt.Product] {
			return
		}
		seen[tt.Product] = true
		tokens = append(tokens, tt.Product)
	}
	for _, r := range rec.AffectedRanges {
		add(r.Package)
	}
	if len(tokens) == 0 {
		add(rec.Title)
	}
	return tokens
}

// cveRangesFromRecord 把情报记录的受影响区间转成 B-05 精判输入（cveintel.Range；
// Package 不进精判——纯求值只消费区间与生态；区间级 ecosystem 缺省回落顶层）。
func cveRangesFromRecord(rec *cveintel.Record) ([]cveintel.Range, string) {
	ranges := make([]cveintel.Range, 0, len(rec.AffectedRanges))
	eco := ""
	for _, a := range rec.AffectedRanges {
		ranges = append(ranges, cveintel.Range{
			From:      a.From,
			To:        a.To,
			Fixed:     a.Fixed,
			Ecosystem: a.Ecosystem,
		})
		if eco == "" && strings.TrimSpace(a.Ecosystem) != "" {
			eco = a.Ecosystem
		}
	}
	return ranges, eco
}

// RunCVEMatchBatch 跑一轮「新 CVE → 反向匹配 → hint」批次（B-04G）：按
// published_after 时间窗枚举（publishedAfter=水位，RFC3339 UTC 由调用方格式化），
// 逐 CVE 提取产品 token → MatchCVEToAssets（真实 cveintel 客户端作精判器——
// B-05 ranges 纯求值经 sidecar，单体零复制判定）→ EmitCVEHints。
//   - CVE_MCP_URL 未配置 ⇒ Unavailable=true 零错误返回（B-01E 静默降级；
//     调用方每轮至多记一次日志）；
//   - 上游故障/协议错误 ⇒ 整批报错（调用方【不推进水位】，下轮重试同窗）；
//   - 窗口饱和（>100 条）时按 sidecar 既有排序（CVSS 降序）优先高价值子集——
//     legacy 面无按时间排序枚举，低 CVSS 滞留项属如实申报的已知取舍（steady
//     state 下 15 分钟窗远小于 100，饱和只在批量导入/首扫出现）；
//   - 批次自身幂等：台账去重在 Emit 层兜底，同窗重跑零重复 hint。
func (s *Server) RunCVEMatchBatch(ctx context.Context, publishedAfter time.Time) (CVEBatchStats, error) {
	var stats CVEBatchStats
	if publishedAfter.IsZero() {
		return stats, fmt.Errorf("cve-batch: publishedAfter 水位必填（绝不全库回扫）")
	}
	cl, err := cveIntelClient()
	if err != nil {
		if strings.Contains(err.Error(), "CVE_MCP_URL") {
			stats.Unavailable = true
			return stats, nil
		}
		return stats, fmt.Errorf("cve-batch: 客户端构造失败: %w", err)
	}
	if s.m == nil || s.m.pg == nil {
		return stats, fmt.Errorf("cve-batch: 数据库未就绪")
	}
	recs, err := cl.SearchRecent(ctx, publishedAfter.UTC().Format(time.RFC3339), cveMatchBatchMax)
	if err != nil {
		return stats, fmt.Errorf("cve-batch: 时间窗枚举失败: %w", err)
	}
	stats.Swept = len(recs)
	for i := range recs {
		rec := &recs[i]
		tokens := cveTokensFromRecord(rec)
		if len(tokens) == 0 {
			stats.NoTokens++ // 无产品信息：绝不猜，跳过
			continue
		}
		ranges, eco := cveRangesFromRecord(rec)
		matches, err := s.m.pg.MatchCVEToAssets(ctx, cl, db.CVEMatchInput{
			CVEID:         rec.CVEID,
			Ecosystem:     eco,
			ProductTokens: tokens,
			Ranges:        ranges,
		})
		if err != nil {
			return stats, fmt.Errorf("cve-batch: CVE %s 匹配失败: %w", rec.CVEID, err)
		}
		stats.MatchedAssets += len(matches)
		emit, err := s.EmitCVEHints(ctx, matches, rec)
		if err != nil {
			return stats, fmt.Errorf("cve-batch: CVE %s hint 发出失败: %w", rec.CVEID, err)
		}
		stats.Emitted += emit.Emitted
		stats.Deduped += emit.Deduped
		stats.Throttled += emit.Throttled
		stats.Skipped += emit.Skipped
	}
	return stats, nil
}
