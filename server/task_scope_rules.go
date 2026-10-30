package server

import (
	"errors"
	"fmt"
)

// evidencePreservationRules 拦住「定向反取证」那一类动作。
//
// 内置的 20 条命令拦截规则（db/db.go 的 seedDefaultInterceptRules 系列）拦的是
// **通用破坏**：rm -rf、DROP、TRUNCATE、shred/wipe 打 /dev、HTTP DELETE、/clear 与
// /purge 之类的批量清空接口。但它们**不拦**定向清理痕迹：改写目标日志、截断日志、
// 抽 systemd journal、批量删日志文件、清 shell 历史、删 wtmp。
//
// 在授权测试里这些动作会毁掉客户付费要保留的取证证据 —— 报告写完才发现日志没了，
// 复盘就没有依据。所以任务可以**逐个开启**证据保全策略，装上下面这批任务级规则。
//
// 每条规则匹配一次工具入参，命中即拦，并把理由回给模型（让它换路子，而不是
// 以为是被目标挡了再去绕）。正则的写法与平台内置规则同款（RE2、大小写不敏感、
// raw 入参 + 归一化入参两个匹配主体）。
var evidencePreservationRules = []struct {
	Name    string
	Pattern string
	Message string
}{
	{
		Name:    "[任务级·证据保全] 改写日志内容",
		Pattern: `(?i)\bsed\b[^|\n]{0,120}-i[^|\n]{0,120}/(var|opt|usr)/log`,
		Message: "证据保全策略禁止改写目标日志内容（sed -i 改日志会破坏取证证据）",
	},
	{
		Name:    "[任务级·证据保全] 截断日志文件",
		Pattern: `(?i)\btruncate\b[^|\n]{0,60}-s\b|(?i)>\s*/(var|opt|usr)/log`,
		Message: "证据保全策略禁止截断目标日志文件",
	},
	{
		Name:    "[任务级·证据保全] 清理 systemd 日志",
		Pattern: `(?i)\bjournalctl\b[^|\n]{0,80}--vacuum`,
		Message: "证据保全策略禁止 journalctl --vacuum 清理系统日志",
	},
	{
		Name:    "[任务级·证据保全] 批量删除日志文件",
		Pattern: `(?i)\bfind\b[^|\n]{0,120}-delete|(?i)\brm\b[^|\n]{0,80}\*\.log`,
		Message: "证据保全策略禁止批量删除日志文件",
	},
	{
		Name:    "[任务级·证据保全] 清除 shell 历史",
		Pattern: `(?i)\bhistory\s+-c\b|(?i)\bunset\s+HISTFILE|(?i)>\s*/\.bash_history`,
		Message: "证据保全策略禁止清除 shell 历史",
	},
	{
		Name:    "[任务级·证据保全] 删除登录/审计记录",
		Pattern: `(?i)\brm\b[^|\n]{0,80}/(var/log/(wtmp|btmp|lastlog)|var/run/utmp)`,
		Message: "证据保全策略禁止删除登录与审计记录文件",
	},
}

// applyEvidencePreservation 给任务装上证据保全策略的命令级拦截规则。
//
// 逐任务开启：没调用它的任务一行规则都不会多，全局命令拦截规则表也不受影响。
// 幂等：重复调用不堆重复行（写入侧的幂等由 db 侧的唯一索引保证）。
//
// 谁调用它：发布任务（POST /api/task-hall/tasks）在 evidence_preservation=true 时。
// 那条接线在 server/task_hall.go（别的单元的文件域），本单元不碰 —— 接线未落地前
// 发布请求里的 evidence_preservation 开关仍会被拒绝，见 server/task_hall.go 里
// "证据保全策略尚未落地" 那条校验。
func (s *Server) applyEvidencePreservation(taskID int64) error {
	if s.m == nil || s.m.pg == nil {
		return errors.New("数据库未连接")
	}
	as := s.m.pg.Assets()
	for _, r := range evidencePreservationRules {
		if err := as.AddTaskInterceptRule(taskID, r.Name, r.Pattern, r.Message); err != nil {
			return fmt.Errorf("安装证据保全规则失败(%s): %w", r.Name, err)
		}
	}
	return nil
}
