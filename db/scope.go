package db

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// platformScopeCTE 把「平台自有资产」物化成与 scopeTargetCTE 同形的 target 关系。
// 只在 assets.origin 列存在时使用。
const platformScopeCTE = `target AS (
  SELECT id FROM assets WHERE origin = 'platform'
)`

// emptyScopeCTE 物化一个恒空的 target —— fail-closed 的唯一形状：
// 没有任务上下文、或平台范围尚未定义时，范围内一个资产都看不到。
const emptyScopeCTE = `target AS (
  SELECT 0 AS id WHERE FALSE
)`

// ScopeFragment 是把资产查询约束到「调用者可见范围」所需的 SQL 片段集合。
//
// CTE 是 WITH 体（不含开头的 "WITH "），它把范围内资产物化成名为 target 的关系；
// Args 是该 CTE 的位置参数（$1..$n，按序）；Predicate 是作用在资产行上的过滤条件，
// 它读 target。
type ScopeFragment struct {
	// CTE 物化范围内资产，关系名固定为 target。
	CTE string
	// Args 是 CTE 的位置参数，按 $1..$n 顺序排列。
	Args []any
	// Predicate 把资产行限制在范围内（引用 target）。
	Predicate string
}

// ScopeFragmentFor 判定某个调用者适用哪套范围，并给出强制它的 SQL。
//
// 这是全平台【唯一】的 scope 判定入口：工具面（agent 的 list_assets 等读接口）与
// 平台 API 面（server 的资产读接口）都从这里拿约束 ⇒ 两边不可能漂移。
//
// taskID > 0  → 该任务（及其直接来源任务）声明的 task_scope（成员关系，不是字面
//                相等：root_domain 范围会带出其下的子域/服务/端点）。
// taskID <= 0 → scopepolicy.Policy.PlatformScopeMode 决定的平台范围，
//                绝不是「全部」。
func (s *AssetStore) ScopeFragmentFor(taskID int64) (ScopeFragment, error) {
	if taskID > 0 {
		return ScopeFragment{
			CTE:       scopeTargetCTE,
			Args:      []any{taskID},
			Predicate: "id IN (SELECT id FROM target)",
		}, nil
	}
	return s.platformScopeFragment()
}

// platformScopeFragment 构造没有任务上下文时的范围。
//
// fail-closed 是这里唯一的默认：模式不是 ScopePlatformOwned、或 assets.origin 列
// 还不存在时，一律返回空范围 —— 不回退全局、不让整个调用失败、也不建列（建列
// 是 DDL 那条线的职责）。少看见资产只是保守，多看见资产是越权。
func (s *AssetStore) platformScopeFragment() (ScopeFragment, error) {
	switch scopepolicy.Policy.PlatformScopeMode {
	case scopepolicy.ScopePlatformOwned:
		hasOrigin, err := s.assetsHaveOriginColumn()
		if err != nil {
			return ScopeFragment{}, fmt.Errorf("读取 assets.origin 列信息失败: %w", err)
		}
		if !hasOrigin {
			// origin 列还不存在 ⇒ 无法区分「平台自有」与「agent 探测得到」⇒ 什么都不给。
			return failClosedScopeFragment(), nil
		}
		return ScopeFragment{CTE: platformScopeCTE, Predicate: "id IN (SELECT id FROM target)"}, nil
	default:
		// 未知模式同样是 fail-closed：宁可少看，不可越权。
		return failClosedScopeFragment(), nil
	}
}

// failClosedScopeFragment 是 fail-closed 的形状：target 恒空。
func failClosedScopeFragment() ScopeFragment {
	return ScopeFragment{CTE: emptyScopeCTE, Predicate: "id IN (SELECT id FROM target)"}
}

// assetsHaveOriginColumn 报告 assets.origin 列是否已经存在。
func (s *AssetStore) assetsHaveOriginColumn() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'assets' AND column_name = 'origin'
)`).Scan(&exists)
	return exists, err
}

// AgentScopeExpansionCap 是一次任务内 agent 自行扩范围的最大条数。
//
// 单一真相源：它与「范围内」判定（ScopeFragmentFor）读的是同一个
// scopepolicy.Policy 值 —— 改那个值，两处行为同时变，不会出现「按一个上限扩、
// 按另一个上限准入」的缝隙。
func AgentScopeExpansionCap() int {
	return scopepolicy.Policy.MaxAgentAddedScope
}

// scopeIDPlaceholders 把资产 id 变成位置占位符，并追加到 args 上。
// 返回新的 args（追加可能换底层数组，调用方必须用返回值）。
func scopeIDPlaceholders(args []any, ids []int64) ([]any, string) {
	base := len(args)
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", base+i+1)
		args = append(args, id)
	}
	return args, strings.Join(placeholders, ",")
}
