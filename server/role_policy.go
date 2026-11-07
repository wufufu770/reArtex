package server

// ---------- 分层经济：role_policy（P2，方案 v3.1 §6）----------
//
// 每角色模型策略：settings 键 `llm.role_policy`（JSON），lane 级解析与额度感知降级。
//
// 策略形状（d2d model-policies 的 {primary, backup} 形状，落 DB settings）：
//
//	{"planner":{"primary":2}, "worker":{"primary":5,"backup":6}, "mainagent":{"primary":1}}
//
// 降级纪律（红队 P0-2/P1-5 修订后的最终形态）：
//   - **决策/验证槽位（planner/mainagent）结构性禁止 backup**：role_policy 校验在
//     settings 写入层直接拒绝 backup≠0 的这两个 lane —— "永不静默降级"不是运行时
//     判断，而是**形状上写不出来**。额度耗尽 = 返回 chainExhausted（暂停），事件
//     llm_paused 照发，人工改档后下一 run 生效。
//   - **执行槽位（worker）**：primary 额度耗尽 → lane 游标推进到 backup（内存态，
//     重启重置回 primary——restart-reset 是刻意简化，文档如实声明）；backup 再
//     耗尽 → 同样 chainExhausted 暂停。
//   - 优先级：显式 agent 绑定 > role_policy > 任务链 > 全局（current() 顺序即证）。
//     role_policy 只在无绑定时接管，不触碰 agentBindingProvider 语义。
//   - **不触碰任务链**：lane 游标是本文件私有的内存状态，MarkTaskLLMProfileQuotaExhausted
//     的任务链推进路径对 role_policy lane 完全不经过（红队 P0-2 的核心修正）。
//
// 事件：复用 emitTaskLLMTransition（llm_switch / llm_failover + 前端 toast 通道）；
// 决策槽位暂停另发 kind='llm_paused'（前端 toast 同款判断，F 线接入）。
//
// 热改闸：role_policy 变更必须走 putSettings 的专用校验（validateRolePolicyJSON），
// 越界形状 400；变更落 audit（红队 P1-5：安全参数不挂无闸热改）。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
)

// RolePolicySettingKey 是 settings 键。
const RolePolicySettingKey = "llm.role_policy"

// RoleLanePolicy 是一个 lane 的模型档（profile id + 可选模型，E1 多模型）。
type RoleLanePolicy struct {
	PrimaryID int64  `json:"primary"`
	PrimaryModel string `json:"primary_model,omitempty"` // 空=该 profile 默认模型
	BackupID  int64  `json:"backup,omitempty"` // planner/mainagent 必须为 0（结构性禁止降级）
}

// RolePolicy 是 settings 里的整份策略（lane → 档）。
type RolePolicy map[string]RoleLanePolicy

// rolePolicyLanes 是可配置的 lane 白名单（对齐引擎四 lane；goals 不纳管——它只做
// 目标拆解一次性调用，模型档走全局即可，避免策略面膨胀）。
// normalizer lane（方案 v3 缺陷 B3）：SoD 规整器的独立模型档。远程实测规整器与
// worker/planner 抢同一个 profile 的限流，37/54 次 unavailable —— 独立 lane 后
// 规整调用不再与主任务链争额度。规整器是「只提议」的基础设施槽位：允许 backup
// （它不裁决，降级不构成安全语义变化；耗尽 = fail-closed 拒绝 + 重试）。
// auditor lane（方案 v4.1 §3）：合并闸裁决岗的模型档——快模型即可，它只裁
// 「放行/拒绝/升级人工」，不产出长文。decisionLanes 不含它：auditor 无任务链
// 语义，backup 语义留给 roleLanePolicy 的通用解析（primary 挂了回活动 profile）。
var rolePolicyLanes = map[string]bool{"planner": true, "worker": true, "mainagent": true, "normalizer": true, "auditor": true}

// decisionLanes 是决策/验证槽位（结构性禁止 backup）。
var decisionLanes = map[string]bool{"planner": true, "mainagent": true}

// ParseRolePolicy 解析并校验 settings 值（JSON 字符串）。返回解析错误（形状/引用）。
// 引用校验（profile 是否存在）由调用方注入 exists 判定 —— 本包不依赖 pg。
func ParseRolePolicy(raw string, profileExists func(int64) bool) (RolePolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RolePolicy{}, nil
	}
	var rp RolePolicy
	if err := json.Unmarshal([]byte(raw), &rp); err != nil {
		return nil, fmt.Errorf("role_policy 不是合法 JSON: %w", err)
	}
	for lane, pol := range rp {
		if !rolePolicyLanes[lane] {
			return nil, fmt.Errorf("未知 lane %q（允许：planner/worker/mainagent/normalizer）", lane)
		}
		if pol.PrimaryID <= 0 {
			return nil, fmt.Errorf("lane %s 的 primary 必须为正的 profile id", lane)
		}
		if decisionLanes[lane] && pol.BackupID != 0 {
			return nil, fmt.Errorf("lane %s 是决策槽位，结构性禁止 backup（额度耗尽=暂停+通知，不自动降级）", lane)
		}
		if pol.BackupID != 0 && pol.BackupID == pol.PrimaryID {
			return nil, fmt.Errorf("lane %s 的 backup 不得与 primary 相同", lane)
		}
		for _, id := range []int64{pol.PrimaryID, pol.BackupID} {
			if id == 0 {
				continue
			}
			if profileExists != nil && !profileExists(id) {
				return nil, fmt.Errorf("lane %s 引用的 profile #%d 不存在", lane, id)
			}
		}
	}
	return rp, nil
}

// roleLanePolicy 读取生效的 lane 策略；未配置该 lane = ok=false。
// 每次调用现读 settings（热改即刻生效；解析失败按未配置处理并记日志——坏配置
// 不能打死引擎，fail-open 到任务链）。
func (s *Server) roleLanePolicy(lane string) (RoleLanePolicy, bool) {
	raw, ok, err := s.m.pg.GetSetting(RolePolicySettingKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return RoleLanePolicy{}, false
	}
	rp, perr := ParseRolePolicy(raw, func(id int64) bool { _, _, ok := s.providerForProfile(id); return ok })
	if perr != nil {
		log.Printf("[role-policy] settings 解析失败（按未配置处理）: %v", perr)
		return RoleLanePolicy{}, false
	}
	pol, ok := rp[lane]
	return pol, ok && pol.PrimaryID > 0
}

// ---------- lane 游标（内存态；重启重置回 primary —— 刻意简化，见文件头）----------

type roleLaneStageKey struct {
	taskID, lane string
}

var (
	roleLaneMu    sync.Mutex
	roleLaneStage = map[roleLaneStageKey]int{} // 0=primary, 1=backup, 2=exhausted(暂停)
)

func roleLaneStageOf(taskID, lane string) int {
	roleLaneMu.Lock()
	defer roleLaneMu.Unlock()
	return roleLaneStage[roleLaneStageKey{taskID, lane}]
}

func setRoleLaneStage(taskID, lane string, stage int) {
	roleLaneMu.Lock()
	defer roleLaneMu.Unlock()
	roleLaneStage[roleLaneStageKey{taskID, lane}] = stage
}

// currentRoleLane 解析 role_policy 桂的当前 provider。
// 返回 ok=false 表示本 lane 无策略或策略已耗尽（耗尽时 err 已带上 chainExhausted）。
// selection.profileID 恒为 0 且 selection.lanePolicy 非空 —— 让 failover 闸知道
// 走 role 分流而不是任务链推进。
func (r *taskLLMRuntime) currentRoleLane(sel *taskLLMSelection) (bool, error) {
	pol, ok := r.s.roleLanePolicy(r.agentKey)
	if !ok {
		return false, nil
	}
	stage := roleLaneStageOf(r.taskID, r.agentKey)
	if stage >= 2 {
		return true, &taskLLMError{taskID: r.taskID, chainExhausted: true,
			cause: fmt.Errorf("role lane %s 暂停：primary 额度耗尽且无 backup（决策槽位等待人工改档）", r.agentKey)}
	}
	pid := pol.PrimaryID
	if stage >= 1 {
		if pol.BackupID == 0 {
			return true, &taskLLMError{taskID: r.taskID, chainExhausted: true,
				cause: fmt.Errorf("role lane %s 额度耗尽且无 backup", r.agentKey)}
		}
		pid = pol.BackupID
	}
	prov, cfg, ok := r.s.providerForProfileModel(pid, pol.PrimaryModel)
	if !ok {
		log.Printf("[role-policy] task %s lane %s 的 profile #%d 不可用，按未配置处理", r.taskID, r.agentKey, pid)
		return false, nil
	}
	sel.provider, sel.retry = prov, cfg.Retry
	sel.lanePolicy = &pol
	return true, nil
}

// rotateRoleLane 是 role_policy lane 的 exhaust 分流：推进 lane 游标并构造
// transition（不触碰任务链表）。stage 语义见 roleLaneStageOf。
func (r *taskLLMRuntime) rotateRoleLane(selection taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
	pol := *selection.lanePolicy
	stage := roleLaneStageOf(r.taskID, r.agentKey)
	next := stage + 1
	if pol.BackupID != 0 && next > 1 {
		next = 1 // worker 档在 backup 上重复耗尽：停在 backup（重试语义），事件照发
	}
	setRoleLaneStage(r.taskID, r.agentKey, next)
	transition := db.TaskLLMTransition{PreviousProfileID: pol.PrimaryID, Advanced: true}
	if next == 1 && pol.BackupID != 0 {
		id := pol.BackupID
		transition.NextProfileID = &id
	} else {
		// 无 backup（决策槽位）或 backup 也耗尽：暂停。
		transition.NextProfileID = nil
		transition.ChainExhausted = true
		transition.Advanced = false
		setRoleLaneStage(r.taskID, r.agentKey, 2)
	}
	log.Printf("[role-policy] task %s lane %s 额度耗尽: stage %d→%d cause=%v",
		r.taskID, r.agentKey, stage, next, cause)
	return transition, nil
}

// emitRoleLanePaused 决策槽位暂停的显式事件（区别于 llm_failover 的"耗尽"）：
// 语义是「按策略暂停，等人工改档」，前端 toast 同款通道可识别 kind。
func (s *Server) emitRoleLanePaused(t *Task, lane string, pol RoleLanePolicy, cause error) {
	if t == nil {
		return
	}
	profile := s.llmAuditProfile(pol.PrimaryID)
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: "paused", Reason: cause.Error(), Previous: profile,
	}})
	summary := fmt.Sprintf("role lane %s 已按策略暂停（%s 额度耗尽）——等待人工改档", lane, llmAuditProfileLabel(profile))
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "llm_paused", IsError: true,
		Summary: summary, Detail: cause.Error(), Metadata: metadata})
	log.Printf("[role-policy] task %s lane %s paused: %v", t.ID, lane, cause)
}

// ValidateRolePolicyForSettings 是 putSettings 的写入闸：形状校验 + 引用校验。
// 决策槽位的 backup 在这里被结构性拒绝（ParseRolePolicy）。
func (s *Server) ValidateRolePolicyForSettings(raw string) error {
	_, err := ParseRolePolicy(raw, func(id int64) bool {
		_, _, ok := s.providerForProfile(id)
		return ok
	})
	return err
}

// rolePolicyRaw 读原样策略串（settings 页回显用；空串=未配置）。
func (s *Server) rolePolicyRaw() string {
	raw, ok, err := s.m.pg.GetSetting(RolePolicySettingKey)
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}
