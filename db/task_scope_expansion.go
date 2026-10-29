// 本文件只管一件事：agent 自主扩范围的上限（写侧）。
//
// 上限读的是 internal/scopepolicy.Policy（单一真相源）—— 与「这个资产在不在范围内」
// 的判定读同一个值，改一处两处同时变。人工设范围走 AddManualScope（db/task_scope.go），
// 不受此限：人工设范围是授权决定，不是扩张预算的一部分。
package db

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// =====================================================================
// agent 自主扩范围的上限（写侧）
// =====================================================================

// ScopeExpansionDeniedError 是「agent 自主扩范围被策略拒绝」的定型错误。
//
// 定型而不是裸字符串：工具层要把拒绝原因回报给 agent（别让它以为扩成功了），
// 审计链也要按结构取用这次拒绝。
type ScopeExpansionDeniedError struct {
	TaskID int64
	Kind   string
	Value  string
	Reason string
}

func (e *ScopeExpansionDeniedError) Error() string { return e.Reason }

// checkAgentScopeKind 判断 kind 是否在 agent 可自行扩范围的 kind 白名单内。
//
// 白名单来自 scopepolicy.Policy.MaxAgentAddedKinds（单一真相源）。刻意不含
// root_domain / company：这两种 kind 会一次性把整棵资产树拉进来（root_domain 范围
// 命中所有 root_domain 列等于它的资产）—— 那是边界被整体推翻，不是「向邻居扩张」，
// 必须由人工决定。
func (s *AssetStore) checkAgentScopeKind(taskID int64, kind, value, toolName string) error {
	for _, allowed := range scopepolicy.Policy.MaxAgentAddedKinds {
		if kind == allowed {
			return nil
		}
	}
	reason := fmt.Sprintf("agent 不得自行把范围扩到 %s：该 kind 会把整棵资产树拉进来（边界被整体推翻），必须由人工确认", kind)
	s.recordScopeExpansionDenied(taskID, kind, value, toolName, reason)
	return &ScopeExpansionDeniedError{TaskID: taskID, Kind: kind, Value: value, Reason: reason}
}

// checkAgentScopeBudget 判定这个任务还剩多少 agent 扩张预算。
//
// 预算按【任务】累计（task_scope 没有 run 维度）：一次任务里 agent 累计新增的范围行数
// （source='auto'/'agent'）不得超过 scopepolicy.Policy.MaxAgentAddedScope —— 与
// db.AgentScopeExpansionCap() 读的是同一个值。人工写的行（source='manual'）不消耗
// 预算：否则人工设一次范围就把 agent 的额度吃光了。
func (s *AssetStore) checkAgentScopeBudget(taskID int64, ts TaskScope, toolName string) error {
	limit := AgentScopeExpansionCap()
	used, exists, err := s.agentScopeUsage(taskID, ts)
	if err != nil {
		return err
	}
	if exists {
		return nil // 同一行已经在库里（幂等重加）⇒ 不消耗预算
	}
	if used >= limit {
		reason := fmt.Sprintf("已达单任务扩权上限（%d 条），本次扩权被丢弃", limit)
		s.recordScopeExpansionDenied(taskID, ts.Kind, scopeRowValue(ts), toolName, reason)
		return &ScopeExpansionDeniedError{TaskID: taskID, Kind: ts.Kind, Value: scopeRowValue(ts), Reason: reason}
	}
	return nil
}

// agentScopeUsage 返回该任务里 agent 侧已写的范围行数，以及本次要写的这条是否已在库里。
func (s *AssetStore) agentScopeUsage(taskID int64, ts TaskScope) (used int, exists bool, err error) {
	companyID := int64(0)
	if ts.CompanyID != nil {
		companyID = *ts.CompanyID
	}
	const query = `
SELECT count(*) FILTER (WHERE source IN ('auto','agent')),
       count(*) FILTER (WHERE source IN ('auto','agent')
         AND kind=$2 AND COALESCE(domain,'')=$3 AND COALESCE(net::text,'')=$4
         AND COALESCE(company_id,0)=$5 AND COALESCE(value,'')=$6)
FROM task_scope WHERE task_id=$1`
	var same int
	if s.tx != nil {
		err = s.tx.QueryRow(query, taskID, ts.Kind, ts.Domain, ts.Net, companyID, ts.Value).Scan(&used, &same)
	} else {
		err = s.db.QueryRow(query, taskID, ts.Kind, ts.Domain, ts.Net, companyID, ts.Value).Scan(&used, &same)
	}
	if err != nil {
		return 0, false, err
	}
	return used, same > 0, nil
}

// recordScopeExpansionDenied 把一次「拒绝扩权」写进审计链。
//
// 为什么必须留痕：拒绝发生在 DB 层，而调用方可能丢弃返回值（自动扩范围那条路径就是
// 截断而不报错）—— 没有审计，「agent 曾经想把自己的范围撑大」事后查不到。
//
// best-effort：审计写失败不能改变调用结果（与 intercept 的处置一致）。
func (s *AssetStore) recordScopeExpansionDenied(taskID int64, kind, value, toolName, reason string) {
	payload, err := json.Marshal(map[string]any{
		"task_id": taskID,
		"kind":    kind,
		"value":   value,
		"cap":     AgentScopeExpansionCap(),
		"kinds":   scopepolicy.Policy.MaxAgentAddedKinds,
	})
	if err != nil {
		payload = []byte("{}")
	}
	if _, err := s.db.AppendAuditEvent(AuditEvent{
		Actor:    "agent",
		ToolName: toolName,
		Action:   "scope_expansion_denied",
		Reason:   reason,
		Payload:  payload,
	}); err != nil {
		log.Printf("[audit] 追加 scope 扩权拒绝审计失败: %v", err)
	}
}

// scopeRowValue 取出范围行的标识值（domain / net / value / company id），用于审计与报错。
func scopeRowValue(ts TaskScope) string {
	switch {
	case ts.Domain != "":
		return ts.Domain
	case ts.Net != "":
		return ts.Net
	case ts.Value != "":
		return ts.Value
	case ts.CompanyID != nil:
		return strconv.FormatInt(*ts.CompanyID, 10)
	}
	return ""
}
