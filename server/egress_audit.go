package server

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// A-01C 纵深：把 netguard 拨号守卫的拒绝补进审计链。
//
// 为什么要有这一批：守卫的拒绝此前只以拨号错误浮出（复测失败/工具报错），
// 审计链里没有「守卫判了 deny」这条判定动作——「没留痕」和「留了痕」在事后
// 追责时长得一模一样。deny 是判定动作，必须进链（approve 量极大不进链，
// 与 db/audit_chain.go 的口径一致）。
//
// 识别口径：netguard 的拒绝错误一律以 "[<策略名>]" 开头——拨号 Control 的
// 「[<名>] 拒绝连接 <ip>（提示）」与 ResolveOnce 的「[<名>] <host> 解析出的
// N 个地址全部被拒（提示）」（netguard.go）。按前缀匹配自己的策略名，不误收
// 别家错误、不冒充守卫判定。

// netguardDenialByPrefix 报告 err 是否为指定策略的守卫拒绝。
func netguardDenialByPrefix(err error, policyName string) bool {
	if err == nil || policyName == "" {
		return false
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "["+policyName+"]") {
		return false
	}
	// 前缀命中还不够：同前缀也可能是「无法解析目标地址」等非判定错误。拒绝面
	// 的两个确定文案（deny 判定动作才进链，不冒充）：
	return strings.Contains(msg, "拒绝连接") || strings.Contains(msg, "全部被拒")
}

// auditNetguardDenial 写一条守卫拒绝审计（best-effort：审计失败不改变调用
// 结果，但必须 log 可见）。
func (s *Server) auditNetguardDenial(policyName, target string, err error) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	payload, merr := json.Marshal(map[string]any{
		"policy": policyName,
		"target": target,
	})
	if merr != nil {
		payload = []byte("{}")
	}
	reason := err.Error()
	if len(reason) > 300 {
		reason = reason[:300] + "…"
	}
	if _, aerr := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "netguard",
		ToolName: policyName,
		Action:   "deny",
		Reason:   reason,
		Payload:  payload,
	}); aerr != nil {
		log.Printf("[netguard-audit] 守卫拒绝留痕失败: %v", aerr)
	}
}
