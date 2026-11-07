package server

// ---------- 收尾审计报告（方案 v4.1 §3：auditor 的旁路产出）----------
//
// 任务落终态时汇总本次运行的裁决账本：硬拦几条、快路径放行几条、auditor
// 裁了几条、升级人工几条、blocked 残留几个——播报到绑定会话 + 引擎活动流。
// 纯账本聚合（SQL count），零 LLM——「实时监控所有操作」的底座是账本。

import (
	"fmt"
	"log"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

// emitAuditSummary 是 Engine.AuditSummary 的落点（best-effort，异步不必——
// 终态点本就是低频路径，同步查询两次 count 可接受）。
func (s *Server) emitAuditSummary(t *Task) {
	if t == nil || t.Store == nil || s.m == nil || s.m.pg == nil {
		return
	}
	numeric, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || numeric <= 0 {
		return
	}
	pg := s.m.pg
	var hardDeny, pass, auditorDecided, elevated, blockedIntents int
	q := func(sql string, dst *int) {
		if err := pg.QueryRow(sql, numeric).Scan(dst); err != nil {
			log.Printf("[audit-summary] task %s 查询失败（跳过该项）: %v", t.ID, err)
		}
	}
	// 锁定基线/种类硬拒。
	q(`SELECT count(*) FROM sod_decisions WHERE task_id=$1 AND verdict='deny'
	   AND reason_code IN ('destructive_action','platform_baseline','category_deny','auditor_deny','ask_declined','auditor_unavailable','category_cap_exceeded')`, &hardDeny)
	// 快路径/auditor/升级放行（checks 里带合并闸标记的 approve 行）。
	q(`SELECT count(*) FROM sod_decisions WHERE task_id=$1 AND verdict='approve'
	   AND (checks->>'category' IS NOT NULL OR checks->>'auditor' IS NOT NULL OR checks->>'elevated_ask' IS NOT NULL)`, &pass)
	q(`SELECT count(*) FROM sod_decisions WHERE task_id=$1 AND checks->>'auditor' IS NOT NULL`, &auditorDecided)
	q(`SELECT count(*) FROM sod_decisions WHERE task_id=$1 AND checks->>'elevated_ask' IS NOT NULL`, &elevated)
	// blocked 残留（v4 §0.7 教训：done 不许掩盖没跑成的意图）。
	if t.Store != nil {
		if ids, err := t.Store.BlockedIntentIDs(); err == nil {
			blockedIntents = len(ids)
		}
	}
	summary := fmt.Sprintf("【操作审计】任务 #%s 收尾：硬拦 %d 条，快路径/裁决放行 %d 条（auditor 参与 %d、人工批 %d），blocked 残留 %d 个意图",
		t.ID, hardDeny, pass, auditorDecided, elevated, blockedIntents)
	if blockedIntents > 0 {
		summary += "——存在未跑成的意图，详情页可 rerun-blocked 重跑"
	}
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: summary})
	// 绑定会话播报（总控/群聊可见）。
	if sess, err := s.m.pg.SessionByTask(numeric); err == nil && sess != nil {
		s.say(sess.ID, numeric, "system", "system", summary)
	}
	_ = hardDeny
	_ = pass
	_ = auditorDecided
	_ = elevated
	_ = blockedIntents
}
