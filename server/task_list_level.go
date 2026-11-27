package server

import (
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// parseTaskListLevelFilter 校验 GET /api/tasks 的难度和评级筛选。
// 查询参数名叫 difficulty，筛的是 tasks.difficulty（任务难度）。
// 它不是 POST /api/tasks 正文里的 difficulty：那个键仍是执行档。
// 空串表示不筛选。grade=ungraded 表示还没评。非法值由调用方返回 400。
func parseTaskListLevelFilter(q url.Values) (difficulty, grade, errMsg string) {
	difficulty = strings.TrimSpace(q.Get("difficulty"))
	if difficulty != "" && !agent.ValidDifficulty(difficulty) {
		return "", "", "difficulty 必须是 easy、medium、hard、expert 之一"
	}
	grade = strings.TrimSpace(q.Get("grade"))
	if grade != "" && grade != db.TaskGradeUngraded && !db.ValidTaskGrade(grade) {
		return "", "", "grade 必须是 S、A、B、C 或 ungraded"
	}
	return difficulty, grade, ""
}
