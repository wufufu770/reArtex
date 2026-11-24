package db

import (
	"testing"
)

// TI-05 领域对象访问层测试（§0.5）：schema 应用、假设指纹去重、事实/证据
// 必填约束、能力执行幂等键去重与终态收尾、scope 水位。

func tiSetup(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// 主成功路径：四类对象各自落库→查询→（假设/执行）状态推进。
func TestTaskIntelligenceObjectsRoundTrip(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("TI-05 领域对象往返", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	// scope 水位：无范围任务为 0。
	if v, err := d.ScopeVersionForTask(tk.ID); err != nil || v != 0 {
		t.Fatalf("空任务 scope 水位应为 0, got %d err=%v", v, err)
	}

	// 假设：插入 → 查询 → 状态推进。
	hid, deduped, err := d.InsertHypothesis(&Hypothesis{
		TaskID: tk.ID, Fingerprint: "fp-1", Statement: "普通用户可能执行越权状态转换",
		Source: "model_diff", SourceType: "model_delta", SourceRef: "biz_layer_diffs#12",
		Confidence: 0.4, CapabilityID: "modeling.business",
	})
	if err != nil || deduped {
		t.Fatalf("首次插入假设失败: id=%d deduped=%v err=%v", hid, deduped, err)
	}
	hs, err := d.ListHypothesesByTask(tk.ID, 0)
	if err != nil || len(hs) != 1 {
		t.Fatalf("列假设: n=%d err=%v", len(hs), err)
	}
	if err := d.UpdateHypothesisStatus(hid, HypothesisValidating); err != nil {
		t.Fatalf("推进 validating: %v", err)
	}
	got, err := d.GetHypothesis(hid)
	if err != nil || got.Status != HypothesisValidating {
		t.Fatalf("取回状态不符: %+v err=%v", got, err)
	}

	// 事实：带来源落库；证据：引用落库并指向假设。
	fid, err := d.InsertFact(&Fact{TaskID: tk.ID, Subject: "登录接口", Predicate: "returns",
		Object: "token", FactKind: FactObserved, SourceType: "traffic", SourceRef: "exchange#42",
		Confidence: 0.9, HypothesisID: hid})
	if err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	eid, err := d.InsertEvidenceRef(&EvidenceRef{TaskID: tk.ID, Kind: "traffic_exchange",
		ContentRef: "exchange#42", ContentHash: "deadbeef", Support: EvidenceSupports, HypothesisID: hid})
	if err != nil {
		t.Fatalf("insert evidence: %v", err)
	}
	if facts, _ := d.ListFactsByTask(tk.ID, 0); len(facts) != 1 || facts[0].ID != fid {
		t.Fatalf("列事实不符: %+v", facts)
	}
	if evs, _ := d.ListEvidenceByTask(tk.ID, 0); len(evs) != 1 || evs[0].ID != eid || evs[0].HypothesisID != hid {
		t.Fatalf("列证据不符: %+v", evs)
	}

	// 能力执行：开行 → 终态收尾 → 列表。
	if _, dedup, err := d.StartCapabilityExecution(&CapabilityExecution{
		TaskID: tk.ID, CapabilityID: "exploration.edge", ExecutionID: "exec-ti05-1",
		IdempotencyKey: "idem-ti05-1", ScopeVersionStart: 0,
	}); err != nil || dedup {
		t.Fatalf("start exec: dedup=%v err=%v", dedup, err)
	}
	if err := d.CompleteCapabilityExecution("exec-ti05-1", CapExecCompleted,
		[]byte(`{"edge_run":1}`), 120, 800, "", 0); err != nil {
		t.Fatalf("complete exec: %v", err)
	}
	execs, err := d.ListCapabilityExecutionsByTask(tk.ID, 0)
	if err != nil || len(execs) != 1 || execs[0].Status != CapExecCompleted {
		t.Fatalf("列执行不符: n=%d err=%v", len(execs), err)
	}
}

// 失败路径：假设指纹去重返回既有行；幂等键去重返回既有执行；无来源事实、
// 无引用证据、非法终态、非法假设状态分别被拒绝。
func TestTaskIntelligenceGuards(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("TI-05 守卫路径", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	// 指纹去重：第二次插入 deduped=true 且不产生第二行。
	id1, _, err := d.InsertHypothesis(&Hypothesis{TaskID: tk.ID, Fingerprint: "fp-dup",
		Statement: "同一条假设", SourceType: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	id2, deduped, err := d.InsertHypothesis(&Hypothesis{TaskID: tk.ID, Fingerprint: "fp-dup",
		Statement: "同一条假设", SourceType: "model_delta"})
	if err != nil || !deduped || id2 != id1 {
		t.Fatalf("指纹去重应返回既有行: id1=%d id2=%d deduped=%v err=%v", id1, id2, deduped, err)
	}
	if hs, _ := d.ListHypothesesByTask(tk.ID, 0); len(hs) != 1 {
		t.Fatalf("去重后应只有 1 行, got %d", len(hs))
	}

	// 无语句拒绝。
	if _, _, err := d.InsertHypothesis(&Hypothesis{TaskID: tk.ID}); err == nil {
		t.Fatal("空 statement 应拒绝")
	}

	// 无来源事实 / 无引用证据拒绝。
	if _, err := d.InsertFact(&Fact{TaskID: tk.ID, Subject: "s"}); err == nil {
		t.Fatal("无来源事实应拒绝")
	}
	if _, err := d.InsertEvidenceRef(&EvidenceRef{TaskID: tk.ID, Kind: "log"}); err == nil {
		t.Fatal("无引用证据应拒绝")
	}

	// 幂等键去重：第二次 StartCapabilityExecution 返回既有行。
	if _, dedup, err := d.StartCapabilityExecution(&CapabilityExecution{
		TaskID: tk.ID, CapabilityID: "validation.deep", ExecutionID: "exec-g-1", IdempotencyKey: "idem-g",
	}); err != nil || dedup {
		t.Fatalf("首次执行应新建: dedup=%v err=%v", dedup, err)
	}
	id, dedup, err := d.StartCapabilityExecution(&CapabilityExecution{
		TaskID: tk.ID, CapabilityID: "validation.deep", ExecutionID: "exec-g-2", IdempotencyKey: "idem-g",
	})
	if err != nil || !dedup {
		t.Fatalf("重试应命中幂等键: id=%d dedup=%v err=%v", id, dedup, err)
	}

	// running 不可作终态写回；非法假设状态拒绝；收尾不存在的执行报错。
	if err := d.CompleteCapabilityExecution("exec-g-1", CapExecRunning, nil, 0, 0, "", 0); err == nil {
		t.Fatal("running 不可当终态收尾")
	}
	if err := d.UpdateHypothesisStatus(id1, "confirmed"); err == nil {
		t.Fatal("非法假设状态应拒绝")
	}
	if err := d.CompleteCapabilityExecution("exec-nope", CapExecFailed, nil, 0, 0, "", 0); err == nil {
		t.Fatal("收尾不存在的执行应报错")
	}
}

// scope 水位随范围插入抬升（scope_version 语义验证）。
func TestScopeVersionWatermark(t *testing.T) {
	d := tiSetup(t)
	tk, err := d.CreateTask("TI-05 scope 水位", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	v0, err := d.ScopeVersionForTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 给任务挂一条 company 范围（与 bizTaskGraphHandler 同款：task_scope kind='company'）。
	companyID, err := d.Companies().UpsertByName("TI-05 scope 水位企业")
	if err != nil {
		t.Skipf("无法建企业（%v）— 跳过水位用例", err)
	}
	defer d.Companies().DeleteCompany(companyID)
	if _, err := d.Exec(`INSERT INTO task_scope(task_id, kind, company_id) VALUES ($1,'company',$2)`,
		tk.ID, companyID); err != nil {
		t.Fatalf("插入范围: %v", err)
	}
	v1, err := d.ScopeVersionForTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1 <= v0 {
		t.Fatalf("插入范围后水位应抬升: %d -> %d", v0, v1)
	}
}
