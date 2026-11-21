package server

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// vetoExportFixture 是 exporter 测试的 DB 级夹具：真实 Server + 测试库，
// 直接走 db 层插 finding/retest/SoD 账本行。
type vetoExportFixture struct {
	s             *Server
	taskRow       *db.Task // db 层任务行（ID 是 int64）
	taskIDInt     int64
	pg            *db.DB
	lastFindingID int64
}

func newVetoExportFixture(t *testing.T) *vetoExportFixture {
	t.Helper()
	s, _ := taskHallTestServer(t)
	row, err := s.m.pg.CreateTask("veto exporter 夹具", "fixture 目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(row.ID) })
	return &vetoExportFixture{s: s, taskRow: row, taskIDInt: row.ID, pg: s.m.pg}
}

func (f *vetoExportFixture) cleanup() {}

func (f *vetoExportFixture) task() *Task {
	// exporter 只消费 t.ID（字符串任务号）——db 层建的任务不经 engine.Run，
	// Manager 内存表里没有，直接构造轻量视图。
	return &Task{ID: strconv.FormatInt(f.taskRow.ID, 10)}
}

// seedFinding 直插 findings 行（status 是 findings 表自有列）。
func (f *vetoExportFixture) seedFinding(vulnclass, status string) {
	var id int64
	if err := f.pg.QueryRow(`
INSERT INTO findings(task_id, vulnclass, name, severity, summary, evidence, worker, status)
VALUES ($1,$2,$3,'low',$4,'fixture evidence','fixture-worker',$5) RETURNING id`,
		f.taskIDInt, vulnclass, "fixture-"+vulnclass, "fixture summary", status).Scan(&id); err != nil {
		panic(err)
	}
	f.lastFindingID = id
}

// seedRetestReproduced 给最近一个 finding 落一条 reproduced 复测结论。
func (f *vetoExportFixture) seedRetestReproduced() {
	if f.lastFindingID <= 0 {
		panic("先 seedFinding")
	}
	if _, err := f.pg.Exec(`
INSERT INTO finding_retests(finding_id, verdict, summary, evidence, snapshot)
VALUES ($1,'reproduced','fixture reproduced','fixture evidence','{}')`, f.lastFindingID); err != nil {
		panic(err)
	}
}

// seedSoDDecision 落一条 SoD 账本行（verdict + reason_code）。
func (f *vetoExportFixture) seedSoDDecision(verdict, reasonCode string) {
	taskID := f.taskIDInt
	_, err := f.pg.RecordSoDDecision(&db.SoDDecision{
		TaskID: &taskID, SourceAgent: "work#1", ToolName: "Bash",
		Verdict: verdict, ReasonCode: reasonCode,
		PayloadDigest: fmt.Sprintf("dig-%s-%s", verdict, reasonCode),
	})
	if err != nil {
		panic(err)
	}
}

var _ = strconv.Itoa
