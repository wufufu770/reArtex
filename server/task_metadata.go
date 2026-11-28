package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

const (
	maxTaskNameRunes           = 200
	maxTaskMetadataRequestSize = 16 << 10
)

// updateTaskMetadata changes list-only task metadata. It intentionally does not
// touch lifecycle state, scheduling, or the task's immutable description/goal.
// 任务难度（C-01B）也走这里：改的是 tasks.difficulty，下次派发现读该列（C-02），
// 不回写已有意图。JSON 字段是 task_difficulty，不是执行档的 difficulty。
func (s *Server) updateTaskMetadata(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, ok := s.m.Task(taskID); !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskMetadataRequestSize)
	var request struct {
		Name             *string `json:"name"`
		Pinned           *bool   `json:"pinned"`
		TaskDifficulty   *string `json:"task_difficulty"`
		DifficultyReason *string `json:"difficulty_reason"`
	}
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if request.Name == nil && request.Pinned == nil && request.TaskDifficulty == nil && request.DifficultyReason == nil {
		writeErr(w, http.StatusBadRequest, "至少需要提供 name、pinned、task_difficulty 或 difficulty_reason")
		return
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "任务名称不能为空")
			return
		}
		if utf8.RuneCountInString(name) > maxTaskNameRunes {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("任务名称最多 %d 个字符", maxTaskNameRunes))
			return
		}
		request.Name = &name
	}
	if request.TaskDifficulty != nil {
		level := strings.TrimSpace(*request.TaskDifficulty)
		if !agent.ValidDifficulty(level) {
			writeErr(w, http.StatusBadRequest, "task_difficulty 必须是 easy、medium、hard、expert 之一")
			return
		}
		request.TaskDifficulty = &level
	}
	if request.DifficultyReason != nil {
		reason := strings.TrimSpace(*request.DifficultyReason)
		request.DifficultyReason = &reason
	}
	oldLevel, oldReason := s.taskDifficultyOf(taskID)
	task, err := s.m.UpdateTaskMetadata(taskID, db.TaskPatch{
		Name: request.Name, Pinned: request.Pinned,
		Difficulty: request.TaskDifficulty, DifficultyReason: request.DifficultyReason,
	})
	if err != nil {
		if errors.Is(err, db.ErrTaskDifficultyInvalid) {
			writeErr(w, http.StatusBadRequest, "task_difficulty 必须是 easy、medium、hard、expert 之一")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	newLevel, newReason := s.taskDifficultyOf(taskID)
	s.auditTaskDifficulty(taskID, oldLevel, oldReason, newLevel, newReason)
	writeJSON(w, http.StatusOK, taskLevelView(taskDTO(task, s.resolvedTaskStatus(task)), task.Goal, task.Description, newLevel, newReason))
}

// normalizeCreateTaskLevel 把创建请求里的 task_difficulty 收成存储指针。
// 空白 = 不传 = 默认 medium（nil）。非法值拒绝，不静默回落。
func normalizeCreateTaskLevel(raw string) (*string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, nil
	}
	if !agent.ValidDifficulty(v) {
		return nil, errors.New("task_difficulty 必须是 easy、medium、hard、expert 之一")
	}
	return &v, nil
}

// taskLevelView 在任务 DTO 上附加已落库的任务难度和建议。
// 键名是 task_difficulty / suggested_difficulty，不用 difficulty：
// 创建请求里的 difficulty 仍是执行档（auto/simple/medium/hard）。
func taskLevelView(dto TaskDTO, goal, description, stored, reason string) map[string]any {
	raw, err := json.Marshal(dto)
	out := map[string]any{}
	if err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	sug, why := agent.SuggestDifficulty(goal, description)
	out["task_difficulty"] = stored
	out["difficulty_reason"] = reason
	out["suggested_difficulty"] = sug
	out["suggested_difficulty_reason"] = why
	return out
}

func (s *Server) taskDifficultyOf(taskID string) (string, string) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return db.TaskDifficultyMedium, ""
	}
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return db.TaskDifficultyMedium, ""
	}
	row, err := s.m.pg.GetTask(id)
	if err != nil || row == nil {
		return db.TaskDifficultyMedium, ""
	}
	return row.Difficulty, row.DifficultyReason
}

// auditTaskDifficulty 在难度或理由真的变化时写审计链。
// 名称/置顶目前没有审计事件，这里只覆盖难度（卡面要求的变更留痕）。
// 写入失败只记日志：难度已经落库，不把审计故障倒成 500（与 monitor 配置保存同款）。
func (s *Server) auditTaskDifficulty(taskID, oldLevel, oldReason, newLevel, newReason string) {
	if oldLevel == newLevel && oldReason == newReason {
		return
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"task_id":        taskID,
		"old_difficulty": oldLevel,
		"new_difficulty": newLevel,
		"old_reason":     oldReason,
		"new_reason":     newReason,
	})
	if err != nil {
		log.Printf("[task] 难度变更审计载荷编码失败 task=%s: %v", taskID, err)
		return
	}
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "tasks",
		Action:   "task.difficulty",
		Source:   "task-api",
		Reason:   fmt.Sprintf("任务 %s 难度 %s → %s", taskID, oldLevel, newLevel),
		Payload:  payload,
	}); err != nil {
		log.Printf("[task] 难度变更审计写入失败（难度已落库）task=%s: %v", taskID, err)
	}
}
