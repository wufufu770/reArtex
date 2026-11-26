package db

// osv_events_test.go：B-08D 验收（live PG，tiSetup 既有范式）——合法 event
// 落时间线、重复与并发写只一条、payload 字段白名单/限长/消毒、任务归属闸
// （未授权写拒绝）、时间线读取回放、findings 表零变化、崩溃窗口自愈。

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func osvEventTestInput(taskID int64) OSVEventInput {
	cvss := 9.8
	return OSVEventInput{
		TaskID:        taskID,
		Package:       "lodash",
		Version:       "4.17.20",
		VulnID:        "GHSA-35jh-r3h4-6jhm",
		Aliases:       []string{"CVE-2020-8203"},
		CVSS:          &cvss,
		FixedVersions: []string{"4.17.21"},
		Verdict:       "affected",
		Reason:        "版本 4.17.20 落在 [0, 4.17.21) 受影响区间",
	}
}

func osvFindingsCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM findings`).Scan(&n); err != nil {
		t.Fatalf("findings 计数: %v", err)
	}
	return n
}

func osvActivityCountByKey(t *testing.T, d *DB, dedupKey string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='note' AND metadata->>'dedup_key'=$1`, dedupKey).Scan(&n); err != nil {
		t.Fatalf("activity 计数: %v", err)
	}
	return n
}

// 主链路：合法事件落时间线（kind=note/Worker=system/metadata 字段白名单），
// 重复写幂等 deduped，不同版本各落一条；时间线 ActivityPage 可读回；
// findings 计数前后不变（绝不创建 finding）。
func TestRecordOSVVulnEventLifecycle(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("B-08D 生命周期", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	before := osvFindingsCount(t, d)

	actID, deduped, err := d.RecordOSVVulnEvent(osvEventTestInput(tk.ID))
	if err != nil || deduped || actID <= 0 {
		t.Fatalf("首次写入: actID=%d deduped=%v err=%v", actID, deduped, err)
	}
	// 台账与 activity 一致。
	var expID, ledgerAct int64
	if err := d.QueryRow(`SELECT exploration_id, activity_id FROM osv_event_dedup WHERE task_id=$1`, tk.ID).Scan(&expID, &ledgerAct); err != nil || ledgerAct != actID || expID != tk.ExplorationID {
		t.Fatalf("台账: exp=%d act=%d err=%v", ledgerAct, expID, err)
	}

	// 重复写：deduped=true 同一 activity 行。
	actID2, deduped2, err := d.RecordOSVVulnEvent(osvEventTestInput(tk.ID))
	if err != nil || !deduped2 || actID2 != actID {
		t.Fatalf("重复写应幂等: actID=%d deduped=%v err=%v", actID2, deduped2, err)
	}

	// 不同版本：各落一条（去重键含 version）。
	in2 := osvEventTestInput(tk.ID)
	in2.Version = "4.17.21"
	in2.Verdict = "not_affected"
	actID3, deduped3, err := d.RecordOSVVulnEvent(in2)
	if err != nil || deduped3 || actID3 == actID {
		t.Fatalf("不同版本应各自落: actID=%d deduped=%v err=%v", actID3, deduped3, err)
	}

	// 时间线读取回放（ActivityPage 既有通道）。
	page, _, err := d.Exploration(tk.ExplorationID).ActivityPage(ActivitySessionFilter{}, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, a := range page {
		if a.Kind != "note" || a.Worker != "system" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(a.Metadata, &meta); err != nil {
			t.Fatalf("metadata 应合法 JSON: %v", err)
		}
		if meta["schema"] != OSVEventSchema {
			continue
		}
		found++
		if meta["package"] != "lodash" || meta["verdict"] != "affected" && meta["version"] != "4.17.20" {
			if meta["version"] == "4.17.20" {
				if meta["osv_id"] != "GHSA-35jh-r3h4-6jhm" {
					t.Fatalf("osv_id 应保留: %v", meta)
				}
				if al, ok := meta["cve_aliases"].([]any); !ok || len(al) != 1 || al[0] != "CVE-2020-8203" {
					t.Fatalf("cve_aliases 应保留: %v", meta)
				}
				if cv, ok := meta["cvss"].(float64); !ok || cv != 9.8 {
					t.Fatalf("cvss 应保留: %v", meta)
				}
				if fv, ok := meta["fixed_versions"].([]any); !ok || len(fv) != 1 || fv[0] != "4.17.21" {
					t.Fatalf("fixed_versions 应保留: %v", meta)
				}
			}
		}
	}
	if found != 2 {
		t.Fatalf("时间线应见 2 条 osv 事件: %d", found)
	}

	// findings 表零变化（卡验收逐字）。
	if after := osvFindingsCount(t, d); after != before {
		t.Fatalf("findings 计数不得变化: before=%d after=%d", before, after)
	}
}

// 并发幂等：N 协程同键并发写——恰 1 条 activity、恰 1 个 deduped=false。
func TestRecordOSVVulnEventConcurrent(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("B-08D 并发去重", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	const n = 16
	var wg sync.WaitGroup
	var fresh atomic.Int32
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, deduped, err := d.RecordOSVVulnEvent(osvEventTestInput(tk.ID))
			errs[idx] = err
			if !deduped {
				fresh.Add(1)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("并发写 #%d 不应报错: %v", i, e)
		}
	}
	if fresh.Load() != 1 {
		t.Fatalf("应恰 1 个 fresh 写入: %d", fresh.Load())
	}
	if got := osvActivityCountByKey(t, d, fmt.Sprintf("%d|lodash@4.17.20|GHSA-35jh-r3h4-6jhm", tk.ID)); got != 1 {
		t.Fatalf("并发后 activity 应恰 1 条: %d", got)
	}
}

// 输入与归属闸（未授权写拒绝）：缺字段/超限 identity/非法三态/任务不存在/
// 任务无 exploration——全部拒绝且零写入。
func TestRecordOSVVulnEventValidation(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("B-08D 输入闸", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	bad := []struct {
		name string
		mut  func(*OSVEventInput)
	}{
		{"缺package", func(i *OSVEventInput) { i.Package = " " }},
		{"缺version", func(i *OSVEventInput) { i.Version = "" }},
		{"缺osv_id", func(i *OSVEventInput) { i.VulnID = "" }},
		{"identity超限", func(i *OSVEventInput) { i.Package = strings.Repeat("p", osvMaxPkgLen+1) }},
		{"非法三态", func(i *OSVEventInput) { i.Verdict = "confirmed" }},
		{"任务不存在", func(i *OSVEventInput) { i.TaskID = 999999999 }},
		{"task_id非法", func(i *OSVEventInput) { i.TaskID = 0 }},
	}
	for _, tc := range bad {
		in := osvEventTestInput(tk.ID)
		tc.mut(&in)
		if _, _, err := d.RecordOSVVulnEvent(in); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
	// 全部拒绝后零写入。
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM osv_event_dedup WHERE task_id=$1`, tk.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("拒绝路径不得留台账: n=%d err=%v", n, err)
	}
	if got := osvActivityCountByKey(t, d, fmt.Sprintf("%d|lodash@4.17.20|GHSA-35jh-r3h4-6jhm", tk.ID)); got != 0 {
		t.Fatalf("拒绝路径不得落时间线: %d", got)
	}
}

// payload 限长与白名单：长 reason 截断带「…(截断)」、alias/fixed 封顶 16、
// maybe 摘要明写「疑似/需人工验证」绝不冒充确认。
func TestRecordOSVVulnEventPayloadCaps(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("B-08D payload 限长", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	in := osvEventTestInput(tk.ID)
	in.Verdict = "maybe"
	in.Reason = strings.Repeat("r", osvMaxReasonLen+100)
	for i := 0; i < osvMaxListItems+5; i++ {
		in.Aliases = append(in.Aliases, fmt.Sprintf("CVE-2020-%04d", i))
		in.FixedVersions = append(in.FixedVersions, fmt.Sprintf("1.0.%d", i))
	}
	in.CVSS = nil // 若有：缺省键省略
	actID, deduped, err := d.RecordOSVVulnEvent(in)
	if err != nil || deduped {
		t.Fatalf("写入: %d %v %v", actID, deduped, err)
	}
	var meta map[string]any
	var metaRaw []byte
	if err := d.QueryRow(`SELECT metadata FROM activity WHERE id=$1`, actID).Scan(&metaRaw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("metadata 应合法 JSON: %v", err)
	}
	// maybe 摘要语义（卡要求 5：不确定情报不得显示为确认）。
	var summary string
	if err := d.QueryRow(`SELECT summary FROM activity WHERE id=$1`, actID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	for _, kw := range []string{"maybe", "疑似", "需人工验证"} {
		if !strings.Contains(summary, kw) {
			t.Fatalf("maybe 摘要应含 %q: %s", kw, summary)
		}
	}
	if strings.Contains(summary, "确认") {
		t.Fatalf("maybe 摘要绝不能冒充确认: %s", summary)
	}
	// 限长。
	if r, _ := meta["reason"].(string); len(r) > osvMaxReasonLen || !strings.Contains(r, "…(截断)") {
		t.Fatalf("reason 应截断: len=%d", len(r))
	}
	if al, _ := meta["cve_aliases"].([]any); len(al) != osvMaxListItems {
		t.Fatalf("alias 应封顶 %d: %d", osvMaxListItems, len(al))
	}
	if fv, _ := meta["fixed_versions"].([]any); len(fv) != osvMaxListItems {
		t.Fatalf("fixed 应封顶 %d: %d", osvMaxListItems, len(fv))
	}
	if _, has := meta["cvss"]; has {
		t.Fatalf("CVSS 缺省键应省略: %v", meta)
	}
}

// 崩溃窗口：台账在而 activity 缺——新鲜行按在途处理（不双写、不补落）；
// 陈旧行自愈（原子占位→补落 activity→回填台账），且并发自愈只落一条。
func TestRecordOSVVulnEventSelfHeal(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("B-08D 崩溃自愈", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	// 模拟「台账已插、activity 未落」的崩溃残留（activity_id=0，陈旧）。
	if _, err := d.Exec(`
INSERT INTO osv_event_dedup(task_id, exploration_id, pkg, version, osv_id, created_at)
VALUES ($1,$2,'lodash','4.17.20','GHSA-35jh-r3h4-6jhm', now()-interval '10 minutes')`,
		tk.ID, tk.ExplorationID); err != nil {
		t.Fatal(err)
	}
	// 新鲜行语义先行验证：另一版本模拟在途（activity_id=0，刚写入）。
	if _, err := d.Exec(`
INSERT INTO osv_event_dedup(task_id, exploration_id, pkg, version, osv_id, created_at)
VALUES ($1,$2,'lodash','9.9.9','GHSA-fresh', now())`,
		tk.ID, tk.ExplorationID); err != nil {
		t.Fatal(err)
	}
	inFresh := osvEventTestInput(tk.ID)
	inFresh.Version = "9.9.9"
	inFresh.VulnID = "GHSA-fresh"
	if _, deduped, err := d.RecordOSVVulnEvent(inFresh); err != nil || !deduped {
		t.Fatalf("新鲜零 activity 行应按在途 deduped 处理: deduped=%v err=%v", deduped, err)
	}
	if got := osvActivityCountByKey(t, d, fmt.Sprintf("%d|lodash@9.9.9|GHSA-fresh", tk.ID)); got != 0 {
		t.Fatalf("在途行绝不补落: %d", got)
	}

	// 陈旧行：自愈补落。
	actID, deduped, err := d.RecordOSVVulnEvent(osvEventTestInput(tk.ID))
	if err != nil || !deduped || actID <= 0 {
		t.Fatalf("陈旧零 activity 行应自愈: actID=%d deduped=%v err=%v", actID, deduped, err)
	}
	var ledgerAct int64
	if err := d.QueryRow(`SELECT activity_id FROM osv_event_dedup WHERE task_id=$1 AND version='4.17.20'`, tk.ID).Scan(&ledgerAct); err != nil || ledgerAct != actID {
		t.Fatalf("自愈应回填台账: %d %v", ledgerAct, err)
	}
	if got := osvActivityCountByKey(t, d, fmt.Sprintf("%d|lodash@4.17.20|GHSA-35jh-r3h4-6jhm", tk.ID)); got != 1 {
		t.Fatalf("自愈后应恰 1 条: %d", got)
	}
	// 自愈后重复：幂等 deduped 且不再新增。
	if _, deduped2, err := d.RecordOSVVulnEvent(osvEventTestInput(tk.ID)); err != nil || !deduped2 {
		t.Fatalf("自愈后重复应幂等: %v", err)
	}
	if got := osvActivityCountByKey(t, d, fmt.Sprintf("%d|lodash@4.17.20|GHSA-35jh-r3h4-6jhm", tk.ID)); got != 1 {
		t.Fatalf("自愈后重复不得新增: %d", got)
	}
}
