package server

// 本文件是策略库（L4）的三个**只读**观测面：策略之间的关系边、utility 物化
// 视图、curator 的仲裁面。
//
// 【只读】本文件一个字都不写：这三样都是【观测到的状态】的出口。裁决写
// （晋级 / 退役 / 墓碑 / 置位解 pin）发生在 server/strategy_curator.go 与
// server/strategy_api.go 里 —— 一个 HTTP GET 绝不触发裁决。

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 三个只读面的单页上限（人工复核面，不需要无界返回）。
const (
	strategyEdgeListLimit    = 500
	strategyUtilityListLimit = 500
	strategyCurationLimit    = 200
)

// strategyEdgeDTO 是一条策略关系边的对外形状。
type strategyEdgeDTO struct {
	FromID   int64  `json:"from_id"`
	ToID     int64  `json:"to_id"`
	Relation string `json:"relation"`
}

// strategyUtilityDTO 是 utility 物化视图一行的对外形状。
type strategyUtilityDTO struct {
	StrategyID    int64   `json:"strategy_id"`
	UtilityRaw    float64 `json:"utility_raw"`
	EvidenceCount int64   `json:"evidence_count"`
}

// strategyCurationEntryDTO 是仲裁面上一条策略的对外形状。
type strategyCurationEntryDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	State         string `json:"state"`
	Pinned        bool   `json:"pinned"`
	RetiredAt     string `json:"retired_at,omitempty"`
	RetiredReason string `json:"retired_reason,omitempty"`
	ExpiredAt     string `json:"expired_at,omitempty"`
}

// strategyEdgeList 读策略之间的关系边。
//
// ?strategy_id=<id> 只列与该策略相关的边（出边 + 入边）；不传 = 全量。
func (s *Server) strategyEdgeList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.EnsureStrategySchema(); err != nil {
		writeErr(w, 500, "读策略关系边失败: "+err.Error())
		return
	}
	sid, ok := sodOptionalInt64(w, r.URL.Query().Get("strategy_id"), "strategy_id")
	if !ok {
		return
	}
	q := `SELECT from_id, to_id, relation FROM strategy_edges`
	args := []any{}
	if sid > 0 {
		q += ` WHERE from_id=$1 OR to_id=$1`
		args = append(args, sid)
	}
	q += ` ORDER BY from_id, to_id LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, strategyEdgeListLimit)
	rows, err := pg.Query(q, args...)
	if err != nil {
		writeErr(w, 500, "读策略关系边失败: "+err.Error())
		return
	}
	defer rows.Close()
	out := []strategyEdgeDTO{}
	for rows.Next() {
		var e strategyEdgeDTO
		if err := rows.Scan(&e.FromID, &e.ToID, &e.Relation); err != nil {
			writeErr(w, 500, "读策略关系边失败: "+err.Error())
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, 500, "读策略关系边失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"edges": out, "count": len(out)})
}

// strategyUtilityList 读 utility 物化视图。
//
// utility 的**权威值**在这个视图里（strategies.utility 只是缓存值）：视图按
// 「证据链 × finding 当前验证状态」算，所以证据被撤回时 utility 会跟着降。
//
// ⚠️ 本端点**不刷新**视图：刷新是写动作（REFRESH MATERIALIZED VIEW），由
// curator 定时做（server/strategy_curator.go 的 RunOnce 第 1 步），操作者想
// 立刻对齐走 POST /api/strategies/refresh-utility。
func (s *Server) strategyUtilityList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.EnsureStrategySchema(); err != nil {
		writeErr(w, 500, "读 utility 视图失败: "+err.Error())
		return
	}
	sid, ok := sodOptionalInt64(w, r.URL.Query().Get("strategy_id"), "strategy_id")
	if !ok {
		return
	}
	q := `SELECT strategy_id, utility_raw, evidence_count FROM strategy_utility`
	args := []any{}
	if sid > 0 {
		q += ` WHERE strategy_id=$1`
		args = append(args, sid)
	}
	q += ` ORDER BY utility_raw DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, strategyUtilityListLimit)
	rows, err := pg.Query(q, args...)
	if err != nil {
		writeErr(w, 500, "读 utility 视图失败: "+err.Error())
		return
	}
	defer rows.Close()
	out := []strategyUtilityDTO{}
	for rows.Next() {
		var u strategyUtilityDTO
		if err := rows.Scan(&u.StrategyID, &u.UtilityRaw, &u.EvidenceCount); err != nil {
			writeErr(w, 500, "读 utility 视图失败: "+err.Error())
			return
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, 500, "读 utility 视图失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"utility": out,
		"count":   len(out),
		"note":    "utility_raw 是视图里的权威 utility（证据链 × finding 当前验证状态）；strategies.utility 只是缓存值。",
	})
}

// strategyCuration 读 curator 的仲裁面。
//
// ⚠️ **不是**某一轮 curator 的运行报告：curator 的报告（CuratorReport）只打进
// 日志，仓库里没有把它持久化的地方 ⇒ 「某一次仲裁做了什么」无处可读。这里给出
// 的是**仲裁面当前的样子** + 裁决留下的**持久痕迹**（被淘汰的策略带着理由、
// 被墓碑的策略带着到期时间、唯一证人的 pin 名单）。
//
// ⚠️ 本端点**不跑** curator：跑一轮就是裁决写（晋级 / 退役 / 置位），那是
// server/strategy_curator.go 的事，不该由一个 GET 触发。
func (s *Server) strategyCuration(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if err := pg.EnsureStrategySchema(); err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	states := map[string]int{}
	rows, err := pg.Query(`SELECT state, count(*) FROM strategies GROUP BY state`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
			return
		}
		states[state] = int(n)
	}
	rows.Close()

	retired, err := s.strategyCurationList(pg, `state='retired'`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	tombstoned, err := s.strategyCurationList(pg, `state='tombstone'`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	pinned, err := s.strategyCurationList(pg, `pinned=true`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	quarantined, err := s.strategyCurationList(pg, `state='quarantined'`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	ttlDue, err := s.strategyCurationList(pg, `expired_at IS NOT NULL AND expired_at<=now() AND state<>'tombstone'`)
	if err != nil {
		writeErr(w, 500, "读策略仲裁面失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"states":      states,
		"retired":     retired,
		"tombstoned":  tombstoned,
		"pinned":      pinned,
		"quarantined": quarantined,
		"ttl_due":     ttlDue,
		// 仲裁面是**只读**的：晋级 / 退役 / 解 pin 只能由 curator 走确定性判据
		// 发生，或由人显式操作（server/strategy_api.go）。这里点任何东西都不
		// 构成裁决。
		"advisory_only": true,
		"note": "这是仲裁面当前的样子与裁决留下的持久痕迹，不是某一轮 curator 的运行报告" +
			"（CuratorReport 只进日志，仓库里没有持久化它的地方）。",
	})
}

// strategyCurationList 按条件列仲裁面上的策略条目。
func (s *Server) strategyCurationList(pg *db.DB, where string) ([]strategyCurationEntryDTO, error) {
	rows, err := pg.Query(`SELECT id, name, state, pinned, retired_at, retired_reason, expired_at
FROM strategies WHERE `+where+` ORDER BY id LIMIT $1`, strategyCurationLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []strategyCurationEntryDTO{}
	for rows.Next() {
		var (
			e          strategyCurationEntryDTO
			retiredAt  *time.Time
			retiredWhy string
			expiredAt  *time.Time
		)
		if err := rows.Scan(&e.ID, &e.Name, &e.State, &e.Pinned, &retiredAt, &retiredWhy, &expiredAt); err != nil {
			return nil, err
		}
		e.RetiredAt = sodTimeRFC(retiredAt)
		e.RetiredReason = retiredWhy
		e.ExpiredAt = sodTimeRFC(expiredAt)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
