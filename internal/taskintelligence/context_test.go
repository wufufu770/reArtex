package taskintelligence

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------- fakes ----------

type fakeScope struct {
	sum ScopeSummary
	err error
}

func (f fakeScope) ScopeSummary(context.Context, int64) (ScopeSummary, error) {
	return f.sum, f.err
}

type fakeAssets struct {
	sum AssetSummary
	err error
}

func (f fakeAssets) AssetSummary(context.Context, int64) (AssetSummary, error) {
	return f.sum, f.err
}

type fakeExploration struct {
	sum ExplorationSummary
	err error
}

func (f fakeExploration) ExplorationSummary(context.Context, int64) (ExplorationSummary, error) {
	return f.sum, f.err
}

type fakeBiz struct {
	sum BizModelSummary
	err error
}

func (f fakeBiz) BizModelSummary(context.Context, int64) (BizModelSummary, error) {
	return f.sum, f.err
}

type fakeFacts struct {
	sum FactsSummary
	err error
}

func (f fakeFacts) FactsSummary(context.Context, int64) (FactsSummary, error) {
	return f.sum, f.err
}

type fakeEvidence struct {
	sum EvidenceSummary
	err error
}

func (f fakeEvidence) EvidenceSummary(context.Context, int64) (EvidenceSummary, error) {
	return f.sum, f.err
}

type fakeFindings struct {
	sum FindingsSummary
	err error
}

func (f fakeFindings) FindingsSummary(context.Context, int64) (FindingsSummary, error) {
	return f.sum, f.err
}

type fakeHistory struct {
	sum HistorySummary
	err error
}

func (f fakeHistory) HistorySummary(context.Context, int64) (HistorySummary, error) {
	return f.sum, f.err
}

// ---------- 主成功路径 ----------

func TestBuildAllSectionsPopulated(t *testing.T) {
	b := &SnapshotBuilder{
		Scope:       fakeScope{sum: ScopeSummary{Version: "scope-v7", AssetRows: 12}},
		Assets:      fakeAssets{sum: AssetSummary{Total: 30, InScope: 12}},
		Exploration: fakeExploration{sum: ExplorationSummary{Open: 3, Dispatched: 2, Disproven: 7}},
		BizModel:    fakeBiz{sum: BizModelSummary{Version: 5, UpdatedAt: time.Now(), LayerDiffs: 2}},
		Facts:       fakeFacts{sum: FactsSummary{Total: 40, Hypotheses: 6}},
		Evidence:    fakeEvidence{sum: EvidenceSummary{Count: 88}},
		Findings:    fakeFindings{sum: FindingsSummary{Open: 1, Confirmed: 3}},
		History:     fakeHistory{sum: HistorySummary{Executions: 25}},
	}
	snap, err := b.Build(context.Background(), 42)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if snap.ContractVersion != ContextContractVersion {
		t.Fatalf("契约号 = %q, 要 %q", snap.ContractVersion, ContextContractVersion)
	}
	if snap.TaskID != 42 || snap.Scope.Version != "scope-v7" || snap.Facts.Hypotheses != 6 {
		t.Fatalf("快照字段回填不完整: %+v", snap)
	}
	if bad := snap.Unavailable(); len(bad) != 0 {
		t.Fatalf("全接线时不应有不可用分节: %v", bad)
	}
}

// ---------- 失败路径：单分节失败不拖垮整份快照 ----------

func TestBuildSingleSectionFailureIsolated(t *testing.T) {
	boom := errors.New("biz 连接超时")
	b := &SnapshotBuilder{
		Scope:    fakeScope{sum: ScopeSummary{Version: "scope-v1", AssetRows: 1}},
		Facts:    fakeFacts{sum: FactsSummary{Total: 3}},
		BizModel: fakeBiz{err: boom},
	}
	snap, err := b.Build(context.Background(), 7)
	if err != nil {
		t.Fatalf("单分节失败不应使 Build 整体失败: %v", err)
	}
	bad := snap.Unavailable()
	// 未接线的 5 个分节（assets/exploration/evidence/findings/history）显式标记
	// 不可用，加上失败的 biz_model，共 6 项；scope/facts 正常回填。
	if len(bad) != 6 {
		t.Fatalf("应有 6 个不可用分节（5 未接线 + 1 失败）, got %d: %v", len(bad), bad)
	}
	if !containsPrefix(bad, "biz_model", "biz_model: biz 连接超时") {
		t.Fatalf("biz_model 失败原因应如实带出, got %v", bad["biz_model"])
	}
	if !containsPrefix(bad, "evidence", "provider 未接线") {
		t.Fatalf("未接线分节应标注接线缺失, got %v", bad["evidence"])
	}
	if snap.Facts.Total != 3 || snap.Scope.Version != "scope-v1" {
		t.Fatalf("其余分节应正常回填: %+v", snap)
	}
}

// ---------- 权限/边界：接线缺失显式可见，非法输入拒绝 ----------

func TestBuildNilProviderMarkedUnavailable(t *testing.T) {
	b := &SnapshotBuilder{Scope: fakeScope{sum: ScopeSummary{Version: "v"}}}
	snap, err := b.Build(context.Background(), 9)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bad := snap.Unavailable()
	if len(bad) != 7 {
		t.Fatalf("7 个未接线分节都应显式标记, got %d: %v", len(bad), bad)
	}
	if !containsPrefix(bad, "exploration", "provider 未接线") {
		t.Fatalf("未接线原因应如实标注, got %v", bad)
	}
}

func TestBuildRejectsInvalidTaskID(t *testing.T) {
	b := &SnapshotBuilder{}
	if _, err := b.Build(context.Background(), 0); err == nil {
		t.Fatal("taskID<=0 应被拒绝")
	}
}

func TestBuildRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &SnapshotBuilder{Scope: fakeScope{sum: ScopeSummary{Version: "v"}}}
	if _, err := b.Build(ctx, 1); err == nil {
		t.Fatal("已取消的 ctx 应使 Build 返回错误")
	}
}

func containsPrefix(m map[string]string, key, prefix string) bool {
	v, ok := m[key]
	return ok && len(v) >= len(prefix) && v[:len(prefix)] == prefix
}
