package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
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
//
//	相等：root_domain 范围会带出其下的子域/服务/端点）。
//
// taskID <= 0 → scopepolicy.Policy.PlatformScopeMode 决定的平台范围，
//
//	绝不是「全部」。
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

// =====================================================================
// host 口径的范围入口（SoD 闸门用）
//
// 为什么要有这个入口：范围判定此前只有【资产口径】—— 「这个资产 id 在不在范围
// 里」。但闸门手上只有一条动作里的一个 host（域名 / IP），没有资产 id。
//
// ⚠️ 它**不新建**范围语义：直接腿与资产腿都读同一份 task_scope，资产腿复用
// 既有的 ScopedAssetIDs（内部走 ScopeFragmentFor）⇒ 判定仍然只有一处真相源。
// 直接腿的 kind 映射与 scopeTargetCTE 的逐条对应，由
// TestScopeHostInScopeAgreesWithScopedAssetIDs 交叉钉住。
// =====================================================================

// ScopeHostInScope 报告某个 host（域名 / IP）是否落在任务的【有效范围】内。
//
// 判定分两腿（缺一不可）：
//
//	① 直接腿：host 命中 task_scope 的 root_domain（含其子域）/ subdomain /
//	   ip / cidr / keyword 行 ⇒ 在范围内。
//	② 资产腿：host 解析成资产 id 后，走【既有】的 ScopedAssetIDs
//	   （db/asset_dsl.go）⇒ 覆盖 company / icp / asset 这三类只能经资产行
//	   表达的 kind。
//
// 两条腿任一命中即在范围内。
//
// fail-closed：查不到范围行 / 范围为空 / host 解析不出来 ⇒ (false, nil)
// （那不是「在范围内」，也不是错误）。出错 ⇒ (false, err) ⇒ 调用方按拒绝处理。
func (s *AssetStore) ScopeHostInScope(taskID int64, host string) (bool, error) {
	normalized := normalizeScopeHost(host)
	if normalized == "" {
		return false, nil
	}
	ok, err := s.scopeHostDirectHit(taskID, normalized)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	ids, err := s.assetIDsForHost(normalized)
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}
	inScope, err := s.ScopedAssetIDs(taskID, ids)
	if err != nil {
		return false, err
	}
	return len(inScope) > 0, nil
}

// ScopeDigestForTask 返回任务【有效范围】的稳定摘要。
//
// 用途：规整器缓存键的 scope 分量 ⇒ **范围一变，缓存必失效**。少了它，一个
// 「范围收紧前生成的规整结果」会被范围收紧后复用 = 范围旁路。
//
// 摘要覆盖：任务本身 + 其直接来源任务的全部 task_scope 行（与
// scopeTargetCTE / directTaskContextCTE 的 context_tasks 同一套语义）。
//
// 口径：kind + 归一化后的值，按 (kind, id) 排序后拼接再取 SHA-256。
// 归一化：domain / value 取小写（范围语义与大小写无关）；net 用 PG 的文本形态
// （cidr 本身已是规范形态）；id 不进内容（只决定顺序）—— 内容只由「范围是什么」
// 决定，不由「这些行什么时候插进来的」决定。
func (s *AssetStore) ScopeDigestForTask(taskID int64) (string, error) {
	rows, err := s.db.Query(`WITH `+directTaskContextCTE+`
SELECT ts.kind, COALESCE(ts.domain,''), COALESCE(ts.net::text,''), COALESCE(ts.value,''),
       COALESCE(ts.company_id,0), COALESCE(ts.asset_id,0)
FROM task_scope ts
JOIN context_tasks ctx ON ctx.task_id=ts.task_id
ORDER BY ts.kind, ts.id`, taskID)
	if err != nil {
		return "", fmt.Errorf("查询任务范围行失败: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var (
			kind, domain, netText, value string
			companyID, assetID           int64
		)
		if err := rows.Scan(&kind, &domain, &netText, &value, &companyID, &assetID); err != nil {
			return "", fmt.Errorf("查询任务范围行失败: %w", err)
		}
		lines = append(lines, strings.Join([]string{
			kind,
			strings.ToLower(strings.TrimSpace(domain)),
			strings.TrimSpace(netText),
			strings.ToLower(strings.TrimSpace(value)),
			fmt.Sprintf("%d", companyID),
			fmt.Sprintf("%d", assetID),
		}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("查询任务范围行失败: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// scopeHostDirectHit 是 host 口径的「直接腿」：只看 task_scope 行本身。
//
// kind 映射与 scopeTargetCTE 逐条对应：
//   - root_domain：$2 等于该域名，或 $2 是它的子域（'.' + domain 后缀）。
//   - subdomain：精确相等（scopeTargetCTE 里就是 a.domain = ts.domain）。
//   - ip / cidr：host 必须是 IP 字面量（try_inet 解析不出来就是 NULL ⇒ 不命中）。
//   - keyword：host 包含该关键词 —— 关键词在资产层没有对应物（关键词从不用来
//     认领资产），它的意义就落在这里。
//
// ⚠️ 用 position() 而不是 LIKE 做关键词匹配：LIKE 会把关键词里的 % / _ 当通配符，
// 一个「%」就能把匹配面撑到任意 host；position 是字面子串匹配，与「关键词」的
// 语义一致。
func (s *AssetStore) scopeHostDirectHit(taskID int64, host string) (bool, error) {
	var hit bool
	err := s.db.QueryRow(`WITH `+directTaskContextCTE+`
SELECT EXISTS (
  SELECT 1
  FROM task_scope ts
  JOIN context_tasks ctx ON ctx.task_id = ts.task_id
  WHERE (ts.kind='root_domain' AND ($2 = lower(ts.domain) OR $2 LIKE '%.' || lower(ts.domain)))
     OR (ts.kind='subdomain'   AND $2 = lower(ts.domain))
     OR (ts.kind IN ('ip','cidr') AND ts.net >>= try_inet($2))
     OR (ts.kind='keyword'     AND position(lower(ts.value) in $2) > 0)
)`, taskID, host).Scan(&hit)
	if err != nil {
		return false, fmt.Errorf("查询 host 范围命中失败: %w", err)
	}
	return hit, nil
}

// assetIDsForHost 把 host 解析成资产 id 列表。
//
// ⚠️ 这条查询**不做范围判定**，只做「host → 资产」的解析；范围判定由调用方的
// ScopedAssetIDs 负责（资产级判定的唯一实现）。若在这里顺手判范围，就多出第二
// 份范围语义。
func (s *AssetStore) assetIDsForHost(host string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM assets
WHERE lower(COALESCE(domain,'')) = $1
   OR COALESCE(ip,'') = $1
   OR lower(COALESCE(root_domain,'')) = $1
   OR strpos(lower(COALESCE(url,'')), $1) > 0`, host)
	if err != nil {
		return nil, fmt.Errorf("查询 host 对应资产失败: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("查询 host 对应资产失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("查询 host 对应资产失败: %w", err)
	}
	return out, nil
}

// normalizeScopeHost 把 host 归一化成可比较的形态。
//
// 小写、去协议头、去用户信息、去路径 / 查询 / 片段、去端口、去尾点、去 IPv6 方括号。
//
// 为什么每一步都要：target_hint 是模型从原始动作里抽出来的自由文本，它可能是
// "https://API.Example.com:8443/path?x=1"，也可能是 "api.example.com."。
// 不归一化就会把同一个目标判成两个 —— 而其中一个可能落在范围外。
func normalizeScopeHost(raw string) string {
	host := strings.TrimSpace(strings.ToLower(raw))
	if host == "" {
		return ""
	}
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	// 先拆端口再拆方括号：IPv6 的 [::1]:80 里方括号包的是地址段。
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimSuffix(host, ".")
}
