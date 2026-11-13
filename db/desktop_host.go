package db

// 桌面控制的宿主授权与工具名口径（0.12.1 A4）。
//
// 这两个东西是闸门分支与拦截种子共用的【唯一语义源】：
//   - DesktopMCPToolPrefix：桌面 MCP 工具的平台全名前缀（mcp__agent-desktop__desktop_）。
//     sod 闸门的桌面识别、拦截种子的名单生成、前端显示都从这里取——三处各写一份
//     会在上游改 server 名时静默漂移。
//   - IsDesktopToolName：判定一个（可能是 ExecuteExtraTool 解包后的内层）工具名
//     是否属于桌面控制面。sod 链在任务路径上用它识别桌面动作（走 HostLocalAction
//     分支或未授权拒绝）。
//   - SetTaskHostTargets / TaskHostTargets：任务级宿主授权集合（窗口/应用清单），
//     闸门第五分支 pass(host_local_action) 的判定依据；空集合 = 未授权（拒绝）。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DesktopMCPToolPrefix 是桌面 MCP 工具的平台全名前缀。
// （server 名固定 agent-desktop——拦截种子 db/desktop_intercept_seed.go 与本常量
// 同源；上游改 server 名时两处必须同步，family coverage 测试会以实测名单卡住。）
const DesktopMCPToolPrefix = "mcp__agent-desktop__desktop_"

// IsDesktopToolName 判定工具名是否属于桌面控制面。
func IsDesktopToolName(tool string) bool {
	return strings.HasPrefix(tool, DesktopMCPToolPrefix)
}

// SetTaskHostTargets 写任务的宿主授权集合（覆盖写；空数组=关闭授权）。
// 调用方负责审计（REST 处理器落 audit_events）。
func (d *DB) SetTaskHostTargets(taskID int64, targets []string) error {
	if taskID <= 0 {
		return fmt.Errorf("task_id 必填")
	}
	clean := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t != "" {
			clean = append(clean, t)
		}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE tasks SET host_targets=$2 WHERE id=$1`, taskID, b)
	return err
}

// TaskHostTargets 读任务的宿主授权集合；任务不存在返回 (nil, false, nil)。
func (d *DB) TaskHostTargets(taskID int64) ([]string, bool, error) {
	var raw []byte
	err := d.QueryRow(`SELECT host_targets FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var targets []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &targets)
	}
	if targets == nil {
		targets = []string{}
	}
	return targets, true, nil
}
