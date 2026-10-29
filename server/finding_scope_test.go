package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// scopeServerFixture 是本文件共用的夹具：两个互不相干的任务、任务 A 名下的一条
// 漏洞，以及「evidence.local」这个 host 的流量（只在任务 A 的范围里）。
type scopeServerFixture struct {
	server   *Server
	finding  *db.RecordedFinding
	taskA    int64
	taskB    int64
	trafficA string // 任务 A 范围内的流量 id
	trafficB string // 任务 B 范围内的流量 id
}

func newScopeServerFixture(t *testing.T) scopeServerFixture {
	t.Helper()
	s, f, _ := trafficEvidenceServer(t)
	if err := s.m.SetTrafficEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.SetTrafficEnabled(false) })

	row, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil || row == nil || row.TaskID == nil {
		t.Fatalf("夹具漏洞缺少归属任务：%v", err)
	}
	taskA := *row.TaskID
	other, err := s.m.CreateTask("scope B", "另一个任务", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskB, _ := strconv.ParseInt(other.ID, 10, 64)
	t.Cleanup(func() { s.m.pg.DeleteTask(taskB) })

	// 任务 A 声明范围：evidence.local 在范围内。
	upsertAsset := func(domain string) int64 {
		t.Helper()
		if _, err := s.m.pg.Exec(`INSERT INTO assets(type,domain,root_domain,last_seen)
VALUES ('subdomain',$1,$1,now()) ON CONFLICT DO NOTHING`, domain); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := s.m.pg.QueryRow(`SELECT id FROM assets WHERE domain=$1 LIMIT 1`, domain).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	upsertAsset("evidence.local")
	if err := s.m.Assets().AddAutoScope(taskA, "subdomain", "evidence.local", "", ""); err != nil {
		t.Fatal(err)
	}
	// 任务 B 声明另一个 host 的范围 —— 两个任务的流量互不可见。
	if err := s.m.Assets().AddAutoScope(taskB, "subdomain", "other.local", "", ""); err != nil {
		t.Fatal(err)
	}
	otherAsset := upsertAsset("other.local")
	t.Cleanup(func() { s.m.pg.Exec(`DELETE FROM assets WHERE id=$1`, otherAsset) })

	seedServerEvidenceFlow(t, s, "scope-a-traffic", []byte("task A body"))
	seedServerEvidenceFlow(t, s, "scope-b-traffic", []byte("task B body"))
	// 任务 B 的流量挂在别的 host 上。
	if _, err := s.m.traffic.DB().Exec(`UPDATE exchanges SET host='other.local',url='http://other.local/' WHERE id='scope-b-traffic'`); err != nil {
		t.Fatal(err)
	}
	return scopeServerFixture{server: s, finding: f, taskA: taskA, taskB: taskB, trafficA: "scope-a-traffic", trafficB: "scope-b-traffic"}
}

// scopeCall 调用一个工具，返回展开后的文本与是否为错误。
func scopeCall(t *testing.T, tool actool.CoreTool, ctx context.Context, input any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(ctx, raw, nil)
	if err != nil {
		t.Fatalf("%s 调用失败：%v", tool.Name(), err)
	}
	return result.Flatten(), result.IsError
}

// scopeHostTool 取出某个名字的主机工具。
func scopeHostTool(t *testing.T, tools []actool.CoreTool, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("缺少工具：%s", name)
	return nil
}

// TestGetFindingTrafficRefusesWithoutTaskContext pins the L3 fail-closed half:
// a caller with no task context has no authorization edge, so it must be
// refused — not served from the global findings table.
func TestGetFindingTrafficRefusesWithoutTaskContext(t *testing.T) {
	fx := newScopeServerFixture(t)

	out, isErr := scopeCall(t, fx.server.toolGetFindingTraffic(), context.Background(), map[string]any{"finding_id": fx.finding.FindingID})
	if !isErr {
		t.Fatalf("没有任务上下文时必须拒绝读取漏洞流量，实际返回：%s", out)
	}
	if !strings.Contains(out, "任务上下文") {
		t.Fatalf("拒绝文案必须说明缺少任务上下文，实际：%s", out)
	}
}

// TestGetFindingTrafficRefusesOtherTask pins the L3 ownership half: a task
// that does not own the finding must not be able to read its traffic.
func TestGetFindingTrafficRefusesOtherTask(t *testing.T) {
	fx := newScopeServerFixture(t)

	other := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskB})
	out, isErr := scopeCall(t, fx.server.toolGetFindingTraffic(), other, map[string]any{"finding_id": fx.finding.FindingID})
	if !isErr {
		t.Fatalf("别的任务不得读取该漏洞的流量，实际返回：%s", out)
	}
	if !strings.Contains(out, "不可读取") {
		t.Fatalf("拒绝文案必须说明不在范围内，实际：%s", out)
	}

	// 自己任务的调用必须照常成功（不能把正常路径一起堵死）。
	own := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskA})
	if out, isErr = scopeCall(t, fx.server.toolGetFindingTraffic(), own, map[string]any{"finding_id": fx.finding.FindingID}); isErr {
		t.Fatalf("本任务的调用必须能读到漏洞流量：%s", out)
	}
}

// TestUpdateFindingReportRefusesCrossTask pins the L4 write guard at the tool
// boundary: a report write must not reach a finding owned by another task.
func TestUpdateFindingReportRefusesCrossTask(t *testing.T) {
	fx := newScopeServerFixture(t)
	tool := fx.server.toolUpdateFindingReport()

	other := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskB})
	out, isErr := scopeCall(t, tool, other, map[string]any{"finding_id": fx.finding.NodeID, "report": "越权报告"})
	if !isErr {
		t.Fatalf("跨任务写报告必须被拒，实际返回：%s", out)
	}
	finding, err := fx.server.m.pg.GetFinding(fx.finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if finding.Report != "" {
		t.Fatalf("被拒的写入不得落库，report=%q", finding.Report)
	}

	own := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskA})
	if out, isErr = scopeCall(t, tool, own, map[string]any{"finding_id": fx.finding.NodeID, "report": "本任务报告"}); isErr {
		t.Fatalf("本任务的写报告必须成功：%s", out)
	}
	finding, err = fx.server.m.pg.GetFinding(fx.finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if finding.Report != "本任务报告" {
		t.Fatalf("本任务报告未落库：%q", finding.Report)
	}
}

// TestUpdateFindingReportRefusesWithoutTaskContext pins the fail-closed half
// of L4 at the tool boundary.
func TestUpdateFindingReportRefusesWithoutTaskContext(t *testing.T) {
	fx := newScopeServerFixture(t)

	out, isErr := scopeCall(t, fx.server.toolUpdateFindingReport(), context.Background(), map[string]any{"finding_id": fx.finding.NodeID, "report": "无上下文报告"})
	if !isErr {
		t.Fatalf("没有任务上下文时必须拒绝写报告，实际返回：%s", out)
	}
	finding, err := fx.server.m.pg.GetFinding(fx.finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if finding.Report != "" {
		t.Fatalf("被拒的写入不得落库，report=%q", finding.Report)
	}
}

// TestTrafficToolsRefuseOutOfScopeHost pins the L2 guarantee at the tool
// boundary: traffic captured for a host outside the caller's scope must not be
// readable, and a caller without a task context must be refused outright.
func TestTrafficToolsRefuseOutOfScopeHost(t *testing.T) {
	fx := newScopeServerFixture(t)
	tools := fx.server.m.HostTools()
	search := scopeHostTool(t, tools, "traffic_search")
	get := scopeHostTool(t, tools, "traffic_get")

	own := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskA})
	other := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskB})

	out, isErr := scopeCall(t, search, own, map[string]any{"host": "evidence.local"})
	if isErr {
		t.Fatalf("本任务范围内的流量必须可读：%s", out)
	}
	out, isErr = scopeCall(t, search, other, map[string]any{"host": "evidence.local"})
	if !isErr {
		t.Fatalf("别的任务不得读取该 host 的流量，实际返回：%s", out)
	}
	if !strings.Contains(out, "范围内") {
		t.Fatalf("拒绝文案必须说明不在范围内，实际：%s", out)
	}
	out, isErr = scopeCall(t, get, other, map[string]any{"id": fx.trafficA})
	if !isErr {
		t.Fatalf("别的任务不得读取该条流量原文，实际返回：%s", out)
	}
	out, isErr = scopeCall(t, search, context.Background(), map[string]any{"host": "evidence.local"})
	if !isErr {
		t.Fatalf("没有任务上下文时必须拒绝读取流量，实际返回：%s", out)
	}
	if !strings.Contains(out, "任务上下文") {
		t.Fatalf("拒绝文案必须说明缺少任务上下文，实际：%s", out)
	}
}
