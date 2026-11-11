package server

// ---------- 记忆晋级门 + 两段式披露的宿主侧 API（X4+X2，方案 v10 §6 T2/T3） ----------
//
// GET  /api/memory/entries?state=&limit=&offset=   治理分页列表（跨 scope）
// POST /api/memory/entries/{id}/promote            host-only 晋级（三门评估先行）
// POST /api/memory/entries/{id}/retire             host-only 退役
//
// 纪律：
//   - **host-only**：agent 侧没有任何工具/路径能把条目转成 active（模型写入即
//     隔离，db.AppendMemoryEntryChecked），治理动作只能从这里进；
//   - **三门评估先行**（X2）：promote 先过 EvaluateMemoryPromotion，不过门
//     返回 409 + 「缺哪门」的确定性清单；无 --force；
//   - **治理动作入哈希链**（X12/M1 同款）：promote/retire 都写 audit_events，
//     source='memory-curation' —— actor/时间/来源三要素齐。

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// memoryCurationSource 是本组治理动作的审计来源标记。
const memoryCurationSource = "memory-curation"

// registerMemoryCurationRoutes 注册记忆治理路由（server.go 的路由装配点调用）。
func (s *Server) registerMemoryCurationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/entries", s.memoryEntryList)
	mux.HandleFunc("POST /api/memory/entries/{id}/promote", s.memoryEntryPromote)
	mux.HandleFunc("POST /api/memory/entries/{id}/retire", s.memoryEntryRetire)
}

// memoryEntryList 是治理页的分页列表：跨 scope、按 state 过滤（空=全部当前行）。
// 只读 —— 列表永远不该有副作用。
func (s *Server) memoryEntryList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	entries, total, err := pg.ListMemoryEntriesByState(state, limit, offset)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"entries": entries,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"state":   state,
	})
}

type memoryCurationRequest struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// decodeMemoryCuration 解析治理请求体并校验操作者身份（治理必须留人）。
func decodeMemoryCuration(w http.ResponseWriter, r *http.Request) (memoryCurationRequest, bool) {
	var req memoryCurationRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return req, false
	}
	if strings.TrimSpace(req.Actor) == "" {
		writeErr(w, 400, "记忆治理必须写明操作人（actor）")
		return req, false
	}
	return req, true
}

// appendMemoryCurationAudit 把治理动作写进 audit_events 哈希链
// （X12 三要素：actor/时间/来源；来源 = memory-curation）。
func appendMemoryCurationAudit(pg *db.DB, actor, action, reason string, entryID int64, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["entry_id"] = entryID
	payload, err := json.Marshal(detail)
	if err != nil {
		payload = []byte(`{}`)
	}
	if _, err := pg.AppendAuditEvent(db.AuditEvent{
		Actor:    actor,
		ToolName: "memory_curation",
		Action:   action,
		Reason:   reason,
		Payload:  payload,
		Source:   memoryCurationSource,
	}); err != nil {
		// 审计失败不回滚治理动作本身（转态已发生），但必须响亮 —— 静默丢审计
		// 等于给「改了不认账」开口子。
		log.Printf("[memory-curation] 审计事件写入失败 action=%s entry=%d: %v", action, entryID, err)
	}
}

// memoryEntryPromote 晋级一条隔离态记忆（host-only，三门评估先行）。
func (s *Server) memoryEntryPromote(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "条目 id 无效")
		return
	}
	req, ok := decodeMemoryCuration(w, r)
	if !ok {
		return
	}
	// 三门评估（X2）：结构完整 / 历史复盘 / 实战证据。不过门 ⇒ 409 + 缺哪门。
	pass, missing := EvaluateMemoryPromotion(pg, id)
	if !pass {
		writeJSON(w, 409, map[string]any{
			"ok":            false,
			"error":         "晋级未过三门评估",
			"missing_gates": missing,
			"entry_id":      id,
		})
		return
	}
	entry, err := pg.PromoteMemoryEntry(id, req.Actor)
	if err != nil {
		if errors.Is(err, db.ErrMemoryEntryNotFound) {
			writeErr(w, 404, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	appendMemoryCurationAudit(pg, req.Actor, "memory_entry_promoted", strings.TrimSpace(req.Reason), id, map[string]any{
		"topic_key": entry.TopicKey,
		"version":   entry.Version,
		"to_state":  entry.State,
	})
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"entry_id":   entry.ID,
		"topic_key":  entry.TopicKey,
		"version":    entry.Version,
		"state":      entry.State,
		"supersedes": id,
	})
}

// memoryEntryRetire 退役一条记忆（host-only，闭合有效区间不删内容）。
func (s *Server) memoryEntryRetire(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "条目 id 无效")
		return
	}
	req, ok := decodeMemoryCuration(w, r)
	if !ok {
		return
	}
	// 退役是能改变可见性的治理动作，理由必填（与 strategyRetire 同纪律）。
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, 400, "人工退役必须写明理由（reason）")
		return
	}
	if err := pg.RetireMemoryEntryHost(id, req.Actor); err != nil {
		if errors.Is(err, db.ErrMemoryEntryNotFound) {
			writeErr(w, 404, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	appendMemoryCurationAudit(pg, req.Actor, "memory_entry_retired", strings.TrimSpace(req.Reason), id, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "entry_id": id, "state": "retired"})
}
