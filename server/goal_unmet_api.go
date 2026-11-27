package server

import (
	"log"

	"github.com/Autumn-27/artex/db"
)

// unmetGoalsForTask 返回这个任务还没了结的目标。
// 读取失败时记日志并返回空切片，不把整份任务详情打成 500。
// 空切片不是 nil，JSON 是 []，避免前端把「没有」和「没返回」混在一起。
func unmetGoalsForTask(pg *db.DB, taskID int64) []db.UnmetGoal {
	if pg == nil || taskID <= 0 {
		return []db.UnmetGoal{}
	}
	goals, err := pg.UnmetGoalsByTask(taskID)
	if err != nil {
		log.Printf("[task] 读取未达成目标失败 task=%d: %v", taskID, err)
		return []db.UnmetGoal{}
	}
	return goals
}
