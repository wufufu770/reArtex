package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// noteTaskGrade 在任务状态已经落库之后补评级。
// 进入终态（done/failed/timeout）且还没评过：按确定性公式算一次并落库。
// 离开终态：清回未评，下一次进入终态再算。已经评过的不随 finding 改动重算。
// 失败只记日志。状态已经写上，不把评级故障倒成调用方的错误。
// 自动这一次不写审计；重算只走 regradeTask。
func noteTaskGrade(pg *db.DB, taskID int64, status string) {
	if pg == nil || taskID <= 0 {
		return
	}
	if db.IsTerminal(status) {
		if err := persistTaskGrade(pg, taskID, false); err != nil {
			log.Printf("[task] 终态评级失败（状态已落库）task=%d status=%s: %v", taskID, status, err)
		}
		return
	}
	if err := pg.ClearTaskGrade(taskID); err != nil {
		log.Printf("[task] 离开终态时清除评级失败 task=%d: %v", taskID, err)
	}
}

// persistTaskGrade 从库里读四项计数，用 ComputeGrade 算出等级并写入。
// force=false 时已有等级不覆盖。不调 LLM。
func persistTaskGrade(pg *db.DB, taskID int64, force bool) error {
	facts, err := pg.LoadTaskGradeFacts(taskID)
	if err != nil {
		return err
	}
	result := agent.ComputeGrade(gradeInput(facts))
	_, err = pg.SaveTaskGrade(taskID, result.Grade, result.Summary, result.Score, force)
	return err
}

func gradeInput(facts db.TaskGradeFacts) agent.GradeInput {
	findings := make([]agent.GradeFinding, len(facts.Findings))
	for i, f := range facts.Findings {
		findings[i] = agent.GradeFinding{
			Summary:                f.Summary,
			Evidence:               f.Evidence,
			EvidenceLevel:          f.EvidenceLevel,
			EvidenceLevelRequested: f.EvidenceLevelRequested,
			EvidenceLevelReason:    f.EvidenceLevelReason,
		}
	}
	return agent.GradeInput{
		GoalsMet:                 facts.GoalsMet,
		GoalsTotal:               facts.GoalsTotal,
		VerifiedFindings:         facts.VerifiedFindings,
		TotalFindings:            facts.TotalFindings,
		CoveredAssets:            facts.CoveredAssets,
		ScopedAssets:             facts.ScopedAssets,
		EvidenceCompleteFindings: agent.CountEvidenceComplete(findings),
	}
}

// regradeTask 是显式重新评级。只接受终态任务，覆盖已有等级，并写审计。
// 审计失败只记日志，不把已落库的评级倒成 500。
func (s *Server) regradeTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	t, ok := s.m.Task(taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未装配")
		return
	}
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	before, err := s.m.pg.GetTask(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if before == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if !db.IsTerminal(before.Status) {
		writeErr(w, http.StatusConflict, "只有终态任务可以重新评级")
		return
	}
	if err := persistTaskGrade(s.m.pg, id, true); err != nil {
		if errors.Is(err, db.ErrTaskGradeNotTerminal) {
			writeErr(w, http.StatusConflict, "只有终态任务可以重新评级")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	after, err := s.m.pg.GetTask(id)
	if err != nil || after == nil {
		writeErr(w, http.StatusInternalServerError, "评级已写入但读回失败")
		return
	}
	s.auditTaskGrade(taskID, before, after)
	level, reason, mode, grade, summary, score := s.taskDifficultyOf(taskID)
	writeJSON(w, http.StatusOK, taskLevelView(taskDTO(t, s.resolvedTaskStatus(t)), t.Goal, t.Description, level, reason, mode, grade, summary, score))
}

// auditTaskGrade 记录一次显式重新评级。自动终态结算不走这里。
// 即使等级没变也记：操作者按了重新评级，就要留痕。
// 写入失败只记日志。
func (s *Server) auditTaskGrade(taskID string, before, after *db.Task) {
	if s == nil || s.m == nil || s.m.pg == nil || before == nil || after == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"task_id":     taskID,
		"old_grade":   before.Grade,
		"new_grade":   after.Grade,
		"old_summary": before.GradeSummary,
		"new_summary": after.GradeSummary,
		"old_score":   scoreJSON(before.GradeScore),
		"new_score":   scoreJSON(after.GradeScore),
	})
	if err != nil {
		log.Printf("[task] 重新评级审计载荷编码失败 task=%s: %v", taskID, err)
		return
	}
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "tasks",
		Action:   "task.grade",
		Source:   "task-api",
		Reason:   fmt.Sprintf("任务 %s 重新评级 %s → %s", taskID, gradeAuditLabel(before.Grade), gradeAuditLabel(after.Grade)),
		Payload:  payload,
	}); err != nil {
		log.Printf("[task] 重新评级审计写入失败（评级已落库）task=%s: %v", taskID, err)
	}
}

func gradeAuditLabel(grade string) string {
	if grade == "" {
		return "未评"
	}
	return grade
}

func scoreJSON(score *float64) any {
	if score == nil {
		return nil
	}
	return *score
}
