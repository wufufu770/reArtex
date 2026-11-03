package db

import (
	"testing"
)

// 记忆检索（混合检索 + RRF + 上下文分级）的验收用例。
//
// 钉住的是三件事：
//   1. **RRF 是 RRF**：融合只吃名次，两路都命中的条目排在只命中一路的前面；
//      同分时按 entry_id 升序（确定性）。
//   2. **降级必须显式**：pgvector 不可用 / 没给查询向量 ⇒ 结果里必须有降级说明，
//      绝不能悄悄退化成"看起来跑了混合检索"。
//   3. **分级不能骗人**：返回数少于 scope 总数 ⇒ 一定是 PARTIAL，绝不是 COMPLETE。
//
// ⚠️ 用例不碰生产库：连接串来自 db/memory_test.go 的派生库工具（同 db 包内）。

func TestFuseRRFRanksOnly(t *testing.T) {
	// 两路：
	//   text   → A(1) B(2) C(3)
	//   vector → C(1) D(2)
	// A 只在 text 里、D 只在 vector 里、C 两路都有 ⇒ C 的分最高（两条 1/(k+r)）。
	lists := []rrfList{
		{name: "text", ranks: map[int64]int{1: 1, 2: 2, 3: 3}},
		{name: "vector", ranks: map[int64]int{3: 1, 4: 2}},
	}
	fused := fuseRRF(lists, MemoryRRFConstant)
	if len(fused) != 4 {
		t.Fatalf("融合结果条数 = %d，期望 4", len(fused))
	}
	if fused[0].EntryID != 3 {
		t.Errorf("两路都命中的条目应排第一，实际第一是 #%d", fused[0].EntryID)
	}
	// 只在一路命中的两个条目：A = 1/(k+1)，D = 1/(k+2) ⇒ A 在 D 前。
	if fused[1].EntryID != 1 {
		t.Errorf("只命中一次的条目应按名次排序，实际第二是 #%d", fused[1].EntryID)
	}
	// 名次与分数都要被记下来（调用方要能看出每条是被哪几路捞到的）。
	byID := map[int64]rrfEntry{}
	for _, f := range fused {
		byID[f.EntryID] = f
	}
	if byID[3].TextRank != 3 || byID[3].VectorRank != 1 {
		t.Errorf("两路名次未记录：text=%d vector=%d", byID[3].TextRank, byID[3].VectorRank)
	}
	if byID[1].VectorRank != 0 || byID[1].TextRank != 1 {
		t.Errorf("单路名次未记录：text=%d vector=%d", byID[1].TextRank, byID[1].VectorRank)
	}
}

func TestFuseRRFDeterministicTieBreak(t *testing.T) {
	// 两个条目分相同（各自在同一路的名次一样）⇒ 必须按 entry_id 升序，
	// 否则同样的输入会给出不同的顺序（结果不可复现）。
	lists := []rrfList{
		{name: "text", ranks: map[int64]int{7: 1, 5: 1}},
	}
	first := fuseRRF(lists, MemoryRRFConstant)
	second := fuseRRF(lists, MemoryRRFConstant)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("融合结果条数不对：%d / %d", len(first), len(second))
	}
	if first[0].EntryID != 5 || second[0].EntryID != 5 {
		t.Errorf("同分必须按 entry_id 升序，实际 %d / %d", first[0].EntryID, second[0].EntryID)
	}
}

func TestFuseRRFSingleListIsMonotonic(t *testing.T) {
	// 单路时融合退化成名次本身（1/(k+r) 单调递减）⇒ 顺序与输入一致。
	lists := []rrfList{
		{name: "text", ranks: map[int64]int{10: 1, 11: 2, 12: 3}},
	}
	fused := fuseRRF(lists, MemoryRRFConstant)
	if len(fused) != 3 {
		t.Fatalf("融合结果条数 = %d，期望 3", len(fused))
	}
	if fused[0].EntryID != 10 || fused[1].EntryID != 11 || fused[2].EntryID != 12 {
		t.Errorf("单路融合顺序错乱：%d %d %d", fused[0].EntryID, fused[1].EntryID, fused[2].EntryID)
	}
}

func TestGradeMemoryRetrieval(t *testing.T) {
	cases := []struct {
		name       string
		scopeTotal int
		returned   int
		wantGrade  string
		wantReason string
	}{
		{"空 scope", 0, 0, MemoryGradeInsufficient, "empty_scope"},
		{"一条没命中", 5, 0, MemoryGradeInsufficient, "no_match"},
		{"全部进上下文", 3, 3, MemoryGradeComplete, "full_coverage"},
		{"全部进上下文（返回多于 scope，scope 缩小过）", 3, 5, MemoryGradeComplete, "full_coverage"},
		{"只进了一部分", 3, 2, MemoryGradePartial, "partial_coverage"},
		{"被上限截断", 100, 10, MemoryGradePartial, "partial_coverage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grade, reason := gradeMemoryRetrieval(tc.scopeTotal, tc.returned)
			if grade != tc.wantGrade {
				t.Errorf("分级 = %q，期望 %q", grade, tc.wantGrade)
			}
			if reason != tc.wantReason {
				t.Errorf("原因码 = %q，期望 %q", reason, tc.wantReason)
			}
		})
	}
}

// memoryRetrievalOpen 打开测试库并确保记忆 DDL 已应用。
func memoryRetrievalOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(memoryTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureMemorySchema(); err != nil {
		t.Fatalf("应用记忆 schema 失败: %v", err)
	}
	return d
}

// memoryRetrievalSeed 造一批带中文标题的记忆。
func memoryRetrievalSeed(t *testing.T, d *DB, scope MemoryScope) {
	t.Helper()
	seeds := []MemoryEntryInput{
		{TopicKey: "hall-quota", Title: "任务大厅额度语义", SummaryLine: "剩余额度与并发占位", Tags: []string{"额度"}},
		{TopicKey: "asset-scan", Title: "资产扫描入口", SummaryLine: "扫描子域与端口", Tags: []string{"资产"}},
		{TopicKey: "hall-empty", Title: "空态文案", SummaryLine: "本群未派发意图"},
	}
	for _, s := range seeds {
		if _, err := d.AppendMemoryEntry(scope, s); err != nil {
			t.Fatalf("写入记忆失败: %v", err)
		}
	}
}

func TestRetrieveMemoryChineseFullText(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)
	memoryRetrievalSeed(t, d, scope)

	// 「大厅」不是「任务大厅」的整词 ⇒ 只有靠 CJK 二元组展开才能命中。
	res, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: scope, Text: "大厅"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("搜「大厅」应当命中「任务大厅额度语义」，实际 0 条")
	}
	if res.Hits[0].TopicKey != "hall-quota" {
		t.Errorf("第一条命中 = %q，期望 hall-quota", res.Hits[0].TopicKey)
	}
	if len(res.Channels) == 0 || res.Channels[0] != "text" {
		t.Errorf("实际跑过的通道 = %v，期望含 text", res.Channels)
	}
	// 没有给查询向量 ⇒ 必须有一条显式降级。
	if !hasDegradation(res, MemoryDegradationNoQueryEmbedding) {
		t.Errorf("没给查询向量时必须有降级说明，实际降级 = %v", res.Degradations)
	}
	// 3 条记忆、命中 1 条 ⇒ PARTIAL（绝不能是 COMPLETE）。
	if res.Grade != MemoryGradePartial {
		t.Errorf("分级 = %q，期望 partial", res.Grade)
	}
	if res.ScopeTotal != 3 {
		t.Errorf("scope 总数 = %d，期望 3", res.ScopeTotal)
	}
}

func TestRetrieveMemoryFullCoverageIsComplete(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)
	memoryRetrievalSeed(t, d, scope)

	// 「大厅」「额度」「意图」「资产」四个词覆盖全部三条记忆。
	res, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: scope, Text: "大厅 额度 意图 资产", Limit: 50})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if res.Grade != MemoryGradeComplete {
		t.Errorf("全部命中时分级 = %q，期望 complete（命中 %d / 共 %d）", res.Grade, res.Returned, res.ScopeTotal)
	}
	if res.Coverage < 1 {
		t.Errorf("覆盖率 = %v，期望 ≥ 1", res.Coverage)
	}
}

func TestRetrieveMemoryTruncationIsPartial(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)
	memoryRetrievalSeed(t, d, scope)

	res, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: scope, Text: "大厅 额度 意图 资产", Limit: 1})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if res.Returned != 1 {
		t.Fatalf("返回条数 = %d，期望 1", res.Returned)
	}
	if res.Grade != MemoryGradePartial {
		t.Errorf("被截断时分级 = %q，期望 partial", res.Grade)
	}
	if !res.Truncated {
		t.Error("被上限截断时 Truncated 必须为真")
	}
	if res.Matched <= res.Returned {
		t.Errorf("命中面 (%d) 必须大于返回面 (%d)", res.Matched, res.Returned)
	}
}

func TestRetrieveMemoryNoMatchIsInsufficient(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)
	memoryRetrievalSeed(t, d, scope)

	res, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: scope, Text: "zzzz-nonexistent-token"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if res.Grade != MemoryGradeInsufficient {
		t.Errorf("没有命中时分级 = %q，期望 insufficient", res.Grade)
	}
	if res.GradeReason != "no_match" {
		t.Errorf("原因码 = %q，期望 no_match", res.GradeReason)
	}
}

func TestRetrieveMemoryEmptyScopeIsInsufficient(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	res, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: scope, Text: "大厅"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if res.Grade != MemoryGradeInsufficient {
		t.Errorf("空 scope 分级 = %q，期望 insufficient", res.Grade)
	}
	if res.GradeReason != "empty_scope" {
		t.Errorf("原因码 = %q，期望 empty_scope", res.GradeReason)
	}
}

func TestRetrieveMemoryRequiresScope(t *testing.T) {
	d := memoryRetrievalOpen(t)
	if _, err := d.RetrieveMemory(MemoryRetrievalQuery{Scope: MemoryScope{Type: "task_hall"}, Text: "x"}); err == nil {
		t.Fatal("未 scope 的检索必须被拒绝")
	}
}

func TestRetrieveMemoryVectorDegradationIsExplicit(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)
	memoryRetrievalSeed(t, d, scope)

	// 给出查询向量：向量通道要么真的跑了（那 channels 里有 vector），要么
	// **显式**降级（degradations 非空）。绝不允许"看起来跑了其实没跑"。
	res, err := d.RetrieveMemory(MemoryRetrievalQuery{
		Scope:     scope,
		Text:      "大厅",
		Embedding: make([]float32, MemoryEmbeddingDim),
	})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	ranVector := false
	for _, c := range res.Channels {
		if c == "vector" {
			ranVector = true
		}
	}
	if !ranVector && len(res.Degradations) == 0 {
		t.Error("向量通道没跑却没有降级说明 ⇒ 悄悄降级了")
	}
	if !ranVector {
		if !hasDegradation(res, MemoryDegradationNoVectorExtension) &&
			!hasDegradation(res, MemoryDegradationNoVectorStore) &&
			!hasDegradation(res, MemoryDegradationNoVectorEntries) {
			t.Errorf("向量未执行时的降级码不对：%v", degradationCodes(res))
		}
	}
	// 无论向量通道跑没跑，全文通道的结果都必须还在。
	if len(res.Hits) == 0 {
		t.Error("全文通道的结果不应因为向量通道不可用而消失")
	}
}

func TestEncodeVectorLiteral(t *testing.T) {
	got, err := encodeVectorLiteral([]float32{1, 0.5, -2})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if got != "[1,0.5,-2]" {
		t.Errorf("编码结果 = %q，期望 [1,0.5,-2]", got)
	}
	if _, err := encodeVectorLiteral(nil); err != nil {
		t.Fatalf("空向量编码失败: %v", err)
	}
}

// hasDegradation 报告结果里有没有某条降级。
func hasDegradation(res *MemoryRetrievalResult, code string) bool {
	for _, d := range res.Degradations {
		if d != nil && d.Code == code {
			return true
		}
	}
	return false
}

// degradationCodes 把降级码列出来（失败时的报错信息用）。
func degradationCodes(res *MemoryRetrievalResult) []string {
	out := make([]string, 0, len(res.Degradations))
	for _, d := range res.Degradations {
		if d != nil {
			out = append(out, d.Code)
		}
	}
	return out
}
