// 本文件是策略库（L4）的 **operator API**：人这一侧（运营 / 复核人）的入口。
//
// 【裁决写只在宿主侧发生，LLM 一行都不参与】
//   - 批准 / 驳回提议、人工置位 / 解除 pin、人工退役 —— 判据全是**状态比 +
//     必填字段**（理由 + 操作人），没有一处问模型；
//   - 「解 pin」与「退役」是能断掉漏洞报告可复现性的动作 ⇒ 理由与操作人进
//     audit_events 哈希链，事后能查「谁做的、为什么」。
//
// 【退役 ≠ 删除】：退役写的是 state='retired' + retired_reason，行与证据链一行
// 不少 —— 墓园与复活的前提就在这里。
//
// 【pin 复审队列只提示、不自动解】：自动解 pin 会砸掉漏洞报告的可复现性 ——
// 通往某个漏洞的唯一已知路径被退役，报告就再也复现不出来 ⇒ 队列永远是
// advisory（本文件里它是**纯读**的，解 pin 只能由人显式做）。
//
// 本文件**不 import** agent / llm 任何一个包：operator 侧的判据不该有模型的
// 位置（同 server/strategy_curator.go 的同款纪律）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为 server 下的两个文件（实现 +
// 测试），不允许再拆；八个 operator 端点共用同一套「理由 + 操作人 + 审计链」的
// 裁决契约，拆开会让「人工动作必须留痕」这道约束切散（同 server 下的
// strategy_curator.go / strategy_attributor.go 的同款处置）。
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 列表接口的单页上限。策略库是人工复核面，不需要无界返回。
const (
	strategyListLimit         = 200
	strategyProposalListLimit = 100
	strategyPinQueueLimit     = 200
)

// strategyProposalDTO 是提议的对外形状。
//
// 为什么要 DTO：db.StrategyProposal 没有 json tag ⇒ 直接序列化会吐出 Go 字段名
// （ID / StrategyID / …），与本仓库其它 API 的 snake_case 口径不一致。
type strategyProposalDTO struct {
	ID            int64           `json:"id"`
	StrategyID    int64           `json:"strategy_id"`
	Route         string          `json:"route"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Reason        string          `json:"reason"`
	DecidedReason string          `json:"decided_reason"`
	CreatedBy     string          `json:"created_by"`
	DecidedBy     string          `json:"decided_by"`
	DecidedAt     *time.Time      `json:"decided_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// strategyEvidenceDTO 是证据链引用的对外形状（同上：db 侧没有 json tag）。
type strategyEvidenceDTO struct {
	RefType       string    `json:"ref_type"`
	RefID         string    `json:"ref_id"`
	ContentDigest string    `json:"content_digest"`
	Role          string    `json:"role"`
	CreatedAt     time.Time `json:"created_at"`
}

// strategyList 列出策略。可按 state 过滤（不传 = 全部）。
//
// ⚠️ 这里**不**按租户过滤：检索路径（agent 侧，db.SearchActiveStrategies）按
// company_id 隔离，而这里面向平台的运营 / 复核人 —— 平台的鉴权是单管理员 JWT，
// 请求里没有租户身份可过滤，给的是全局盘点。
// ⚠️ 若将来平台引入多租户登录态，这个列表与详情都必须补上 company_id 过滤
// （db.ListStrategies 刻意不带租户过滤，租户口径留给调用方）。
func (s *Server) strategyList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", db.StrategyQuarantined, db.StrategyActive, db.StrategyRetired, db.StrategyTombstone:
	default:
		writeErr(w, 400, "state 只能是 quarantined / active / retired / tombstone")
		return
	}
	items, err := pg.ListStrategies(state, strategyListLimit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"strategies": items})
}

// strategyGet 读一条策略的详情（含证据链）。
//
// 证据链是策略的出处 —— 没有它，一条策略就只是「凭空提权」的主张。
func (s *Server) strategyGet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "策略 id 无效")
		return
	}
	item, err := pg.GetStrategy(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if item == nil {
		writeErr(w, 404, "策略不存在")
		return
	}
	links, err := pg.StrategyEvidence(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	evidence := make([]strategyEvidenceDTO, 0, len(links))
	for _, l := range links {
		evidence = append(evidence, strategyEvidenceDTO{
			RefType:       l.RefType,
			RefID:         l.RefID,
			ContentDigest: l.ContentDigest,
			Role:          l.Role,
			CreatedAt:     l.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"strategy": item, "evidence": evidence})
}

// strategyProposalList 列出提议。status 不传 = 只看待裁决的。
//
// ⚠️ 默认只列 pending：待裁决的提议是这份清单的主要用途（「现在有什么等着我
// 批」），把它和历史混在一起会让复核人看不到该批什么。要看历史就显式传
// approved / rejected。
func (s *Server) strategyProposalList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", db.StrategyProposalPending, db.StrategyProposalApproved,
		db.StrategyProposalRejected, db.StrategyProposalSuperseded:
	default:
		writeErr(w, 400, "status 只能是 pending / approved / rejected / superseded")
		return
	}
	items, err := pg.ListStrategyProposals(status, strategyProposalListLimit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]strategyProposalDTO, 0, len(items))
	for _, p := range items {
		out = append(out, strategyProposalDTO{
			ID:            p.ID,
			StrategyID:    p.StrategyID,
			Route:         p.Route,
			Payload:       p.Payload,
			Status:        p.Status,
			Reason:        p.Reason,
			DecidedReason: p.DecidedReason,
			CreatedBy:     p.CreatedBy,
			DecidedBy:     p.DecidedBy,
			DecidedAt:     p.DecidedAt,
			CreatedAt:     p.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"proposals": out})
}

// strategyProposalDecide 批准 / 驳回一条提议。
//
// 裁决是**裁决写**：只从宿主侧发生，且必须写明裁决人与理由 —— 理由与裁决人
// 一起进 audit_events 哈希链（由 db.DecideStrategyProposal 落链：actor + reason
// + 提议 id 都在载荷里）。
//
// ⚠️ 这里**不**改策略状态：批准一条提议不等于策略生效，生效要过 curator 的
// 晋级三条件（重放闸门 + 观测数 + 里程碑命中）。把「批准」与「生效」合成一步
// 等于绕过那道闸门。
func (s *Server) strategyProposalDecide(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	pid, ok := pathInt(r, "pid")
	if !ok || pid <= 0 {
		writeErr(w, 400, "提议 id 无效")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Status != db.StrategyProposalApproved && req.Status != db.StrategyProposalRejected {
		writeErr(w, 400, "裁决结果只能是 approved 或 rejected")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, 400, "裁决必须写明理由")
		return
	}
	if strings.TrimSpace(req.Actor) == "" {
		writeErr(w, 400, "裁决必须写明操作人")
		return
	}
	// 预检：把「不存在」(404) 与「已裁决」(409) 分开。
	//
	// 为什么要在裁决前查一次：db.DecideStrategyProposal 对这两种情况给的是同一句
	// 错误（「提议 %d 不存在或已裁决」），复核人需要知道到底是哪一种。
	if err := pg.EnsureStrategyProposalSchema(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var current string
	switch err := pg.QueryRow(`SELECT status FROM strategy_proposals WHERE id=$1`, pid).Scan(&current); {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, 404, "提议不存在")
		return
	case err != nil:
		writeErr(w, 500, err.Error())
		return
	case current != db.StrategyProposalPending:
		writeErr(w, 409, fmt.Sprintf("提议已裁决（当前状态 %s）", current))
		return
	}
	if err := pg.DecideStrategyProposal(pid, req.Status,
		strings.TrimSpace(req.Actor), strings.TrimSpace(req.Reason)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// F5（方案 v10 §12+§16）人工裁决侧消账：奇点循环矛盾沉淀的 proposal
	// （created_by="contradiction"，server/contradiction.go F3 建）被批准 ⇒
	// MarkContradictionResolved + CloseGap。best-effort：裁决已落库并进审计链，
	// 消账失败只记日志不影响本次返回。
	if req.Status == db.StrategyProposalApproved {
		s.settleContradictionProposal(pid, req.Reason)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": pid, "status": req.Status})
}

// strategyPinSet 人工置位 / 解除 pin。
//
// 解除必须留理由与操作人，进 audit_events 哈希链 —— 「解 pin」是能断掉漏洞报告
// 可复现性的动作，不能没有痕迹。
//
// ⚠️ 置位与解除都是**确定性**的：这里没有判据也没有模型，只有「操作人说了什么」
// 与「操作人为什么」。自动通道（curator 的 EvaluatePin：唯一证人置位、替代
// 路径 / 漏洞不再 confirmed 两条自动解除）走的是另一条路，两条路都写同一条
// 审计链。
func (s *Server) strategyPinSet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "策略 id 无效")
		return
	}
	var req struct {
		Pinned *bool  `json:"pinned"`
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// pinned 用指针：缺字段与「显式置 false」必须分得开 —— 少了这个区分，一次
	// 漏字段的请求会被当成「解除唯一证人」。
	if req.Pinned == nil {
		writeErr(w, 400, "必须写明 pinned（true=置位，false=解除）")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, 400, "人工置位 / 解除 pin 必须写明理由")
		return
	}
	if strings.TrimSpace(req.Actor) == "" {
		writeErr(w, 400, "人工置位 / 解除 pin 必须写明操作人")
		return
	}
	current, err := pg.GetStrategy(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if current == nil {
		writeErr(w, 404, "策略不存在")
		return
	}
	actor := strings.TrimSpace(req.Actor)
	reason := strings.TrimSpace(req.Reason)
	if *req.Pinned {
		// 置位：operator 的人工通道（带操作人与理由、进审计链）。
		if err := pg.SetStrategyPinned(id, true, actor, reason); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": id, "pinned": true, "changed": !current.Pinned})
		return
	}
	// 解除：走 curator 的第三条解除路径（ApplyStrategyPin + appendPinAudit）——
	// 与自动的那两条解除条件共用同一条审计链。
	if err := NewCurator(pg).ReleasePin(actor, reason, id); err != nil {
		if !current.Pinned {
			// 本来就没被 pin：ReleasePin 仍然把这次操作记进了审计链，只是状态没变。
			// 这不是失败 —— 操作人的动作照样留痕。
			writeJSON(w, 200, map[string]any{
				"ok": true, "id": id, "pinned": false, "changed": false,
				"note": "策略当前没有被 pin；本次操作已记入审计链",
			})
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "pinned": false, "changed": true})
}

// strategyRetire 人工退役一条策略。
//
// 退役 ≠ 删除（反模式 5 的正面对策）：state=retired + retired_at + retired_reason，
// 行还在、证据链还在 —— 墓园与复活的前提就在这里。db.RetireStrategy 负责写
// 状态与审计链。
func (s *Server) strategyRetire(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "策略 id 无效")
		return
	}
	var req struct {
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, 400, "人工退役必须写明理由")
		return
	}
	if strings.TrimSpace(req.Actor) == "" {
		writeErr(w, 400, "人工退役必须写明操作人")
		return
	}
	current, err := pg.GetStrategy(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if current == nil {
		writeErr(w, 404, "策略不存在")
		return
	}
	// 只允许从隔离态 / 在役态退役：已经 retired 的再退役一次没有意义，
	// tombstone 是终态（硬 TTL 到了），人工不该把它往前拨。
	if current.State != db.StrategyQuarantined && current.State != db.StrategyActive {
		writeErr(w, 409, fmt.Sprintf("策略当前状态是 %s，不可退役", current.State))
		return
	}
	if err := pg.RetireStrategy(id, strings.TrimSpace(req.Actor), strings.TrimSpace(req.Reason)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "state": db.StrategyRetired})
}

// strategyPinQueue 唯一证人复审队列。
//
// ⚠️ **只读**：本 handler 一个字都不写 —— 自动解 pin 会砸掉漏洞报告的可复现性
// （通往某个漏洞的唯一已知路径被退役，报告就再也复现不出来）⇒ 队列永远是
// advisory，解 pin 只能由人显式做（POST /api/strategies/{id}/pin）。
//
// 阈值（db.StrategyPinQueueThreshold）与 curator 用的是同一个：堆积到 50 条就
// 该让人看一眼了。这里把它作为 review_required 标记返回 —— 「看」不设门槛，
// 「解」才设。
func (s *Server) strategyPinQueue(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	items, err := pg.PinnedStrategies(strategyPinQueueLimit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total, err := pg.CountPinnedStrategies()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"queue":           items,
		"pinned_count":    total,
		"threshold":       db.StrategyPinQueueThreshold,
		"review_required": total > db.StrategyPinQueueThreshold,
		"advisory_only":   true,
	})
}

// strategyRefreshUtility 手动刷新 utility 物化视图。
//
// 权威 utility 在物化视图里（静态列只是缓存）；curator 平时会自己刷，这个
// 入口给 operator 一个「立刻对齐」的手段。
func (s *Server) strategyRefreshUtility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.RefreshStrategyUtility(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
