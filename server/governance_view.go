package server

// 治理/审计聚合视图（方案 v10 §9 E1-6 / E1-8）——此前系统审计事件散在会话流与
// thinking 里、audit_events 全仓没有明细页、生效 hook 链不可见。本文件只读聚合，
// 不新开写面。

import (
	"net/http"
	"strconv"
)

// taskGovernance 是任务详情「治理」tab 的聚合载荷。
type taskGovernance struct {
	TaskID       string           `json:"task_id"`
	Decisions    []map[string]any `json:"decisions"`    // 本任务 sod_decisions（近 100 条，倒序）
	Overrides    []map[string]any `json:"overrides"`    // 本任务人工改判凭据
	SystemEvents []map[string]any `json:"system_events"` // B6 升级/LLM 故障转移等系统事件（近 100 条）
}

// taskGovernance GET /api/tasks/{id}/governance
func (s *Server) taskGovernance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad task id")
		return
	}
	tid := strconv.FormatInt(id, 10)
	pg := s.pg(w)
	if pg == nil {
		return
	}
	out := taskGovernance{TaskID: tid}

	decRows, err := pg.Query(`
SELECT id, to_char(created_at,'HH24:MI:SS'), COALESCE(source_agent,''), tool_name, verdict,
       COALESCE(reason_code,''), COALESCE(left(note,160),''),
       COALESCE(checks->>'manual_override',''), COALESCE(checks->>'category','')
FROM sod_decisions WHERE task_id=$1 ORDER BY id DESC LIMIT 100`, id)
	if err == nil {
		defer decRows.Close()
	}
	for decRows != nil && decRows.Next() {
		var rid int64
		var ts, agent, tool, verdict, reason, note, overridden, category string
		if decRows.Scan(&rid, &ts, &agent, &tool, &verdict, &reason, &note, &overridden, &category) == nil {
			out.Decisions = append(out.Decisions, map[string]any{
				"id": rid, "ts": ts, "agent": agent, "tool": tool, "verdict": verdict,
				"reason_code": reason, "note": note, "overridden": overridden, "category": category,
			})
		}
	}

	if ovs, oerr := pg.ListSodManualOverrides(id, 100); oerr == nil {
		for _, o := range ovs {
			out.Overrides = append(out.Overrides, map[string]any{
				"id": o.ID, "tool": o.ToolKey, "scope": o.Scope, "reason": o.Reason,
				"created_by": o.CreatedBy, "payload_digest": o.PayloadDigest,
			})
		}
	}

	if t, exists := s.m.Task(tid); exists && t != nil && t.Store != nil {
		actRows, aerr := pg.Query(`
SELECT to_char(created_at,'HH24:MI:SS'), worker, kind, COALESCE(left(summary,200),'')
FROM activity
WHERE exploration_id=$1 AND (worker='system' OR kind IN ('llm_switch','llm_failover','llm_paused'))
ORDER BY id DESC LIMIT 100`, t.ExpID)
		if aerr == nil {
			defer actRows.Close()
		}
		for actRows != nil && actRows.Next() {
			var ts, worker, kind, summary string
			if actRows.Scan(&ts, &worker, &kind, &summary) == nil {
				out.SystemEvents = append(out.SystemEvents, map[string]any{
					"ts": ts, "worker": worker, "kind": kind, "summary": summary,
				})
			}
		}
	}

	if out.Decisions == nil {
		out.Decisions = []map[string]any{}
	}
	if out.Overrides == nil {
		out.Overrides = []map[string]any{}
	}
	if out.SystemEvents == nil {
		out.SystemEvents = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

// auditEvents GET /api/audit/events?page=&size= —— audit_events 明细分页（只读）。
func (s *Server) auditEvents(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 50
	}
	var total int
	_ = pg.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&total)
	rows, err := pg.Query(`
SELECT id, seq, actor, tool_name, action, COALESCE(reason,''), COALESCE(left(payload,240),''),
       digest, prev_digest, to_char(created_at,'MM-DD HH24:MI:SS')
FROM audit_events ORDER BY seq DESC LIMIT $1 OFFSET $2`, size, (page-1)*size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, seq int64
		var actor, tool, action, reason, payload, digest, prev, ts string
		if rows.Scan(&id, &seq, &actor, &tool, &action, &reason, &payload, &digest, &prev, &ts) == nil {
			items = append(items, map[string]any{
				"id": id, "seq": seq, "actor": actor, "tool": tool, "action": action,
				"reason": reason, "payload": payload, "digest": digest, "prev_digest": prev, "ts": ts,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "size": size})
}

// hookChain GET /api/sod/hookchain —— 生效 hook 链的只读描述（E1-8：看得见才能改）。
// 可热改面（种类/拦截规则/信任规则/提示词）指向既有配置端点；代码常量（基线/tier）
// 只读展示并写明理由——安全纪律不进热改面（v4.1 已批）。
func (s *Server) hookChain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"editable": []map[string]string{
			{"name": "拦截种类（动作/每任务 cap）", "endpoint": "/sod/categories", "note": "sod_categories：hard_deny/ask/auditor/pass 与 cap，改后即时生效"},
			{"name": "拦截规则（allow/ask/deny）", "endpoint": "/system/intercept", "note": "intercept_rules：工具级规则与超时动作"},
			{"name": "信任规则（allow 沉淀）", "endpoint": "/system/intercept", "note": "[信任] 前缀 allow 规则；Tier3 与基线特征不可沉淀"},
			{"name": "agent 提示词（含收尾词）", "endpoint": "/system/agents", "note": "agent_prompts：DB 化热改，改后下轮生效"},
			{"name": "裁决 cap 调整", "endpoint": "治理工具 sod_adjust_cap", "note": "监督者/人工可改单一种类 cap"},
		},
		"readonly": []map[string]string{
			{"name": "锁定基线", "reason": "删除/破坏/平台凭据/平台自身配置——安全地板，任何机制不可豁免（v4.1 §10-F1 代码常量）"},
			{"name": "Tier 分级清单", "reason": "Tier1 只读白名单 / Tier2 结构化写 / Tier3 出网·扩权·状态迁移·宿主文件——fail-closed 纪律，热改=污染底层；范围内出网的展示降标见 E1-4"},
			{"name": "SoD 链五检查", "reason": "规整→基线→范围→种类→auditor，每次调用全跑；缓存只存规整结果不存裁决"},
		},
	})
}
