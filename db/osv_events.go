// osv_events.go：OSV 依赖命中 → 任务时间线事件持久化与去重（B-08D）。
//
// 唯一目标：把 OSV 命中（B-08C 三态判定结果）作为任务时间线事件持久化；
// 同一 (task, package@version, OSV ID) 只产生一次；不创建 finding。
//
// 事件通道纪律（卡要求 1/2 逐字）：经核实复用现有 activity 时间线通道
// （ExplorationStore.AppendActivity——前端 transcript 渲染器既有面），
// kind="note" + Worker="system"（与 server/assets.go 平台侧留痕同款先例）。
// 全仓后端与前端均无 "osv_vulns" kind（grep 核实）——原卡提到的 osv_vulns
// kind 不经核实直接写入会落进前端未定义渲染面，故不用（卡要求 1 的「不能
// 未经核实直接写入」逐字）。OSV payload 走 activity.metadata 结构化承载。
//
// payload 纪律（卡要求 3 逐字）：仅包含 package、version、osv_id、
// cve_aliases、cvss（若有，缺省键省略）、fixed_versions、verdict/reason
// ——字段固定白名单=结构性脱敏（无自由键、无凭据面），过 SanitizeEventPayload
// 消毒门纵深（TI-12 存储单点）；identity 字段超限拒绝、reason 超限截断带
// 「…(截断)」、alias/fixed 各封顶 16 条（如实截断，不静默膨胀）。
//
// 去重纪律（卡要求 4 逐字）：osv_event_dedup 表 UNIQUE(task_id,pkg,version,
// osv_id) 由存储层保证并发幂等（ON CONFLICT DO NOTHING）；activity 落库失败
// 即补偿删除台账行（可重试）；崩溃窗口（台账在而 activity 缺）经 metadata
// dedup_key 探测自愈，>60s 才自愈且以原子 UPDATE 占位（-1）防并发双写——
// 在途写入绝不双落。
//
// 三态纪律（卡要求 5 逐字）：maybe 是不确定情报——Summary 明写「疑似/需
// 人工验证」，绝不显示为确认 finding；本文件零 findings 写入（测试断言
// findings 计数不变）；verdict 必须过 B-05 三态白名单，否则拒绝写。
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// OSVEventSchema 是本事件 metadata 的结构版本（增量演进锚点）。
const OSVEventSchema = "osv_event.v1"

// 预算封顶（确定性）。
const (
	osvMaxPkgLen    = 256              // identity 字段：超限拒绝（截断会制造去重键碰撞）
	osvMaxVerLen    = 128              // 同上
	osvMaxVulnIDLen = 128              // 同上
	osvMaxReasonLen = 1024             // 展示文本：超限截断带「…(截断)」
	osvMaxListItems = 16               // cve_aliases / fixed_versions 各自封顶
	osvMaxItemLen   = 128              // 列表单项超限截断
	osvHealAfter    = 60 * time.Second // 台账无 activity 且超此时长才自愈
	osvHealInFlight = -1               // activity_id 占位：自愈进行中（并发可见）
)

// DDL 内联（不另立 .sql 文件——本卡允许清单恰 2 个 go 文件；幂等 IF NOT
// EXISTS，旧数据零触碰：新表纯加法，activity 既有数据与索引不动）。
const osvEventsSchemaSQL = `
CREATE TABLE IF NOT EXISTS osv_event_dedup (
    id BIGSERIAL PRIMARY KEY,
    task_id BIGINT NOT NULL,
    exploration_id BIGINT NOT NULL,
    pkg TEXT NOT NULL,
    version TEXT NOT NULL,
    osv_id TEXT NOT NULL,
    activity_id BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_osv_event_dedup UNIQUE (task_id, pkg, version, osv_id)
);`

var osvEventsSchemaReady sync.Map

// EnsureOSVEventsSchema 幂等建表（bg_task/sod 同范式：进程内按连接记一次，
// SQL 本身幂等，失败不写 memo 下次重试）。
func (d *DB) EnsureOSVEventsSchema() error {
	if _, ok := osvEventsSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(osvEventsSchemaSQL); err != nil {
		return fmt.Errorf("应用 osv 事件去重 schema 失败: %w", err)
	}
	osvEventsSchemaReady.Store(d.DB, struct{}{})
	return nil
}

func (d *DB) ensureOSVEventsSchema() error { return d.EnsureOSVEventsSchema() }

// OSVEventInput 是一条 OSV 命中事件（B-08C 判定结果 + 归属任务）。
type OSVEventInput struct {
	TaskID        int64
	Package       string
	Version       string
	VulnID        string // OSV ID（GHSA/…）
	Aliases       []string
	CVSS          *float64 // 若有；nil=缺省键省略
	FixedVersions []string
	Verdict       string // B-05 三态：affected / not_affected / maybe
	Reason        string
}

// RecordOSVVulnEvent 持久化一条 OSV 命中到任务时间线（activity，kind=note）
// 并按 (task, package@version, osv_id) 去重。返回 (activityID, deduped, err)：
// deduped=true 表示该键已有事件（幂等命中，不重复落时间线）。
func (d *DB) RecordOSVVulnEvent(in OSVEventInput) (int64, bool, error) {
	if err := d.ensureOSVEventsSchema(); err != nil {
		return 0, false, err
	}
	// ① 输入闸（fail-closed）：identity 字段缺一不可、超限拒绝（截断会制造
	// 去重键碰撞——两个不同长包名截断成同键）；verdict 必须过 B-05 三态白名单。
	pkg := strings.TrimSpace(in.Package)
	ver := strings.TrimSpace(in.Version)
	vid := strings.TrimSpace(in.VulnID)
	if in.TaskID <= 0 {
		return 0, false, fmt.Errorf("osv 事件缺 task_id")
	}
	if pkg == "" || ver == "" || vid == "" {
		return 0, false, fmt.Errorf("osv 事件缺 package/version/osv_id")
	}
	if len(pkg) > osvMaxPkgLen || len(ver) > osvMaxVerLen || len(vid) > osvMaxVulnIDLen {
		return 0, false, fmt.Errorf("osv 事件 identity 字段超限（package≤%d/version≤%d/osv_id≤%d——超限拒绝绝不截断 identity）",
			osvMaxPkgLen, osvMaxVerLen, osvMaxVulnIDLen)
	}
	verdict := dbVerdictFromOSVEvent(in.Verdict)
	if verdict == "" {
		return 0, false, fmt.Errorf("osv 事件 verdict %q 非 B-05 三态（affected/not_affected/maybe）——拒绝写", in.Verdict)
	}
	reason := osvTruncate(strings.TrimSpace(in.Reason), osvMaxReasonLen)
	aliases := osvCapList(in.Aliases)
	fixed := osvCapList(in.FixedVersions)

	// ② 任务归属闸：任务必须存在且挂 exploration（时间线 FK 面）——未授权
	// （任务不存在/已删除/无 exploration）一律拒绝，绝不另找落点绕开事件层。
	tk, err := d.GetTask(in.TaskID)
	if err != nil {
		return 0, false, fmt.Errorf("osv 事件任务查询失败: %w", err)
	}
	if tk == nil {
		return 0, false, fmt.Errorf("osv 事件任务不存在或已删除（task %d）——拒绝写", in.TaskID)
	}
	if tk.ExplorationID <= 0 {
		return 0, false, fmt.Errorf("osv 事件任务 %d 无 exploration——无处挂时间线，拒绝写", in.TaskID)
	}
	store := d.Exploration(tk.ExplorationID)

	// ③ payload（卡要求 3 字段白名单；dedup_key 供崩溃自愈探测）。
	meta := map[string]any{
		"schema":    OSVEventSchema,
		"package":   pkg,
		"version":   ver,
		"osv_id":    vid,
		"verdict":   verdict,
		"reason":    reason,
		"dedup_key": fmt.Sprintf("%d|%s@%s|%s", in.TaskID, pkg, ver, vid),
	}
	if len(aliases) > 0 {
		meta["cve_aliases"] = aliases
	}
	if len(fixed) > 0 {
		meta["fixed_versions"] = fixed
	}
	if in.CVSS != nil {
		meta["cvss"] = *in.CVSS
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return 0, false, fmt.Errorf("osv 事件 payload 构造失败: %w", err)
	}
	// 消毒门纵深（TI-12 单点复用）：固定白名单字段本无凭据面，仍过消毒
	// ——键名/形态异常在这里被改写或拒绝。
	clean, _, err := SanitizeEventPayload(raw)
	if err != nil {
		return 0, false, fmt.Errorf("osv 事件 payload 消毒拒绝: %w", err)
	}
	summary := osvEventSummary(pkg, ver, vid, verdict)

	// ④ 台账先行（存储层并发幂等：UNIQUE ON CONFLICT DO NOTHING）。
	var ledgerID int64
	insertErr := d.QueryRow(`
INSERT INTO osv_event_dedup(task_id, exploration_id, pkg, version, osv_id)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (task_id, pkg, version, osv_id) DO NOTHING
RETURNING id`, in.TaskID, tk.ExplorationID, pkg, ver, vid).Scan(&ledgerID)
	if insertErr == nil {
		// 新键：落 activity；失败即补偿删台账（可重试——绝不留死键）。
		actID, aerr := appendOSVActivity(store, summary, clean)
		if aerr != nil {
			if _, derr := d.Exec(`DELETE FROM osv_event_dedup WHERE id=$1`, ledgerID); derr != nil {
				return 0, false, fmt.Errorf("osv 事件落时间线失败(%v)且台账补偿失败(%v)——键可能残留，重试将按幂等处理", aerr, derr)
			}
			return 0, false, fmt.Errorf("osv 事件落时间线失败（已补偿，可重试）: %w", aerr)
		}
		if _, uerr := d.Exec(`UPDATE osv_event_dedup SET activity_id=$2 WHERE id=$1`, ledgerID, actID); uerr != nil {
			// activity 已落而台账 activity_id 未记：崩溃窗口语义（自愈探测兜底），
			// 本调用按成功返回。
			return actID, false, nil
		}
		return actID, false, nil
	}
	if insertErr != sql.ErrNoRows {
		return 0, false, fmt.Errorf("osv 事件台账写入失败: %w", insertErr)
	}

	// ⑤ 幂等命中：读台账状态分派。
	var existID, actID int64
	var createdAt time.Time
	if err := d.QueryRow(`
SELECT id, activity_id, created_at FROM osv_event_dedup
WHERE task_id=$1 AND pkg=$2 AND version=$3 AND osv_id=$4`,
		in.TaskID, pkg, ver, vid).Scan(&existID, &actID, &createdAt); err != nil {
		return 0, false, fmt.Errorf("osv 事件台账回读失败: %w", err)
	}
	if actID > 0 {
		return actID, true, nil // 已完整落库：幂等去重
	}
	if actID == osvHealInFlight {
		return 0, true, nil // 他方自愈进行中：按已存在处理（绝不双写）
	}
	// activity_id==0：台账在而 activity 缺。新鲜行=并发写入可能在途——按已
	// 存在处理（卡要求 4：并发只落一条）；陈旧行（>osvHealAfter）=原写入方
	// 已死，走自愈：原子占位后补落 activity。
	if time.Since(createdAt) < osvHealAfter {
		return 0, true, nil
	}
	var claimed int64
	cErr := d.QueryRow(`
UPDATE osv_event_dedup SET activity_id=$2
WHERE id=$1 AND activity_id=0
RETURNING activity_id`, existID, osvHealInFlight).Scan(&claimed)
	if cErr != nil {
		if cErr == sql.ErrNoRows {
			return 0, true, nil // 被并发抢先（完成或占位）：按已存在处理
		}
		return 0, false, fmt.Errorf("osv 事件自愈占位失败: %w", cErr)
	}
	// 先探测：activity 可能已落而台账未记（activity_id 更新丢失的崩溃窗口）。
	var probeID int64
	pErr := d.QueryRow(`
SELECT id FROM activity
WHERE exploration_id=$1 AND kind='note' AND metadata->>'dedup_key'=$2
ORDER BY id DESC LIMIT 1`, tk.ExplorationID, fmt.Sprintf("%d|%s@%s|%s", in.TaskID, pkg, ver, vid)).Scan(&probeID)
	if pErr == nil {
		if _, uerr := d.Exec(`UPDATE osv_event_dedup SET activity_id=$2 WHERE id=$1`, existID, probeID); uerr != nil {
			return probeID, true, nil
		}
		return probeID, true, nil
	}
	if pErr != sql.ErrNoRows {
		// 探测失败：释放占位，按可重试错误返回。
		_, _ = d.Exec(`UPDATE osv_event_dedup SET activity_id=0 WHERE id=$1`, existID)
		return 0, false, fmt.Errorf("osv 事件自愈探测失败（已释放占位，可重试）: %w", pErr)
	}
	actID2, aerr := appendOSVActivity(store, summary, clean)
	if aerr != nil {
		_, _ = d.Exec(`UPDATE osv_event_dedup SET activity_id=0 WHERE id=$1`, existID) // 释放占位，可重试
		return 0, false, fmt.Errorf("osv 事件自愈落时间线失败（占位已释放，可重试）: %w", aerr)
	}
	if _, uerr := d.Exec(`UPDATE osv_event_dedup SET activity_id=$2 WHERE id=$1`, existID, actID2); uerr != nil {
		return actID2, true, nil
	}
	return actID2, true, nil
}

// appendOSVActivity 经既有通道落一条 note（Worker=system——平台侧留痕先例）。
func appendOSVActivity(store *ExplorationStore, summary string, metadata []byte) (int64, error) {
	return store.AppendActivity(Activity{
		Worker:   "system",
		Kind:     "note",
		Summary:  summary,
		Metadata: json.RawMessage(metadata),
	})
}

// osvEventSummary 组装时间线摘要：affected/maybe 明写「线索/需人工验证」——
// 绝不显示为确认 finding（卡要求 5 逐字）；not_affected 如实陈述判定来源。
func osvEventSummary(pkg, ver, vid, verdict string) string {
	switch verdict {
	case string(VerdictAffected):
		return fmt.Sprintf("OSV 依赖情报：%s@%s 命中 %s（B-05 判定 affected）——情报线索，需人工验证", pkg, ver, vid)
	case string(VerdictMaybe):
		return fmt.Sprintf("OSV 依赖情报：%s@%s 关联 %s（判定 maybe 疑似）——不确定情报，需人工验证", pkg, ver, vid)
	default:
		return fmt.Sprintf("OSV 依赖情报：%s@%s × %s 判定 not_affected（B-05 区间求值）", pkg, ver, vid)
	}
}

// dbVerdictFromOSVEvent 三态白名单（B-05 命名；其余返回空串=拒绝）。
func dbVerdictFromOSVEvent(v string) string {
	switch strings.TrimSpace(v) {
	case string(VerdictAffected):
		return string(VerdictAffected)
	case string(VerdictNotAffected):
		return string(VerdictNotAffected)
	case string(VerdictMaybe):
		return string(VerdictMaybe)
	}
	return ""
}

// osvCapList 列表封顶（条数+单项长度，超限如实截断——只保留前 osvMaxListItems
// 条有效项；截断事实在 reason 之外不另加键，字段白名单纪律优先）。
func osvCapList(in []string) []string {
	out := make([]string, 0, osvMaxListItems)
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, osvTruncate(s, osvMaxItemLen))
		if len(out) >= osvMaxListItems {
			break
		}
	}
	return out
}

// osvTruncate 超限截断带「…(截断)」（B-08A jsTruncateSpec 同纪律）。
func osvTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= len("…(截断)") {
		return s[:max]
	}
	return s[:max-len("…(截断)")] + "…(截断)"
}
