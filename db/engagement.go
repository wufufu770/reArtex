package db

import (
	"errors"
	"strings"
)

// 任务授权面（EngagementPolicy，0.15.26）。
//
// 规划来源（XIB-Artex 自治计划 Phase 2 的 engagement policy 本仓化）：把「人工参与」
// 从逐调用审批上移到【任务级一次性声明】——建任务时声明本任务的授权边界档位，
// 合并闸在自动放行出口按档位收紧（合同内自动放行，合同外先敲门）。三档语义：
//
//	authorized_active —— 默认档 = 现状全部行为（范围内主动测试自动进行）；
//	passive_recon     —— 一切对目标的出网动作（含被动查询）先经人工确认；
//	strict_manual     —— 除平台内安全读写外一律先经人工确认。
//
// 与执行档位（difficulty/mode，0.15.17）的分工：那管「用多少算力」，本列管
// 「允许自动做什么」。两者正交，可独立切换。
//
// 存储纪律：落 tasks 列（持久、重启不丢、审计可查），不放 settings 热改面——
// 授权边界是安全面，单任务粒度校验 + 枚举 CHECK 约束（schema.sql）双保险。
// 判定侧收紧在 internal/sod 合并闸（宿主注入 EngagementProfileOf），本文件只做
// 存储 + 枚举，不承担闸门语义。
const (
	// EngagementAuthorizedActive 是默认档：现状全部行为，零行为变化。
	EngagementAuthorizedActive = "authorized_active"
	// EngagementPassiveRecon：一切对目标的出网动作先经人工确认。
	EngagementPassiveRecon = "passive_recon"
	// EngagementStrictManual：除平台内安全读写外一律先经人工确认。
	EngagementStrictManual = "strict_manual"
)

// ValidEngagementProfile 报告 profile 是否为合法枚举值。
func ValidEngagementProfile(profile string) bool {
	switch profile {
	case EngagementAuthorizedActive, EngagementPassiveRecon, EngagementStrictManual:
		return true
	}
	return false
}

// NormalizeEngagementProfile 归一化：空白/空串回落默认档；合法值原样返回；
// 非法值返回错误（授权面是安全面，坏值不静默回落——调用方决定 400 还是忽略）。
func NormalizeEngagementProfile(profile string) (string, error) {
	p := strings.TrimSpace(profile)
	if p == "" {
		return EngagementAuthorizedActive, nil
	}
	if !ValidEngagementProfile(p) {
		return "", errors.New("engagement_profile 必须是 authorized_active|passive_recon|strict_manual 之一（收到 " + p + "）")
	}
	return p, nil
}

// TaskEngagementProfile 读任务的授权面档位。查不到/未设置一律回落默认档
// （authorized_active = 现状行为）：读失败不收紧也不放宽——授权收紧是显式
// 写入的结果，读侧不做任何创造性解释。
func (d *DB) TaskEngagementProfile(taskID int64) string {
	if d == nil || d.DB == nil || taskID <= 0 {
		return EngagementAuthorizedActive
	}
	var p string
	err := d.QueryRow(`SELECT engagement_profile FROM tasks WHERE id=$1`, taskID).Scan(&p)
	if err != nil || !ValidEngagementProfile(p) {
		return EngagementAuthorizedActive
	}
	return p
}

// SetTaskEngagementProfile 写任务授权面。非法值返回错误（不静默回落）。
func (d *DB) SetTaskEngagementProfile(taskID int64, profile string) error {
	p, err := NormalizeEngagementProfile(profile)
	if err != nil {
		return err
	}
	if d == nil || d.DB == nil {
		return errors.New("数据库未装配")
	}
	ct, err := d.Exec(`UPDATE tasks SET engagement_profile=$2 WHERE id=$1`, taskID, p)
	if err != nil {
		return err
	}
	if n, _ := ct.RowsAffected(); n == 0 {
		return errors.New("任务不存在")
	}
	return nil
}
