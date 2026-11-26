// cve_feed.go：CVE Feed 只读后端接口（B-07A）。
//
// 唯一目标：GET /api/cve/feed?days=7 —— 聚合最近 CVE（经 B-01D 客户端调
// B-03C published_after，绝不读 sidecar SQLite 文件）、CVSS/KEV、资产匹配
// （B-04B 粗筛 + B-04C 精判，内部经 CandidateAssetsByTokens→MatchCVEToAssets
// 既有链）与任务关联（未终态归属 + cve_hints 台账只读 hinted 标记）。本接口
// GET 只读：无任何 hint 写入、不创建任务、不扩大 scope——CVE 情报永远只是
// 线索，立项验证仍走既有 hint→planner 人工链路。
//
// 语义纪律：
//   - days 只允许 3/7/30（缺省 7），其他 400 fail-closed（绝不静默纠偏）；
//   - CVE_MCP_URL 未配置或上游不可达 ⇒ 200 {unavailable:true, items:[]}
//     （B-01E 显式 unavailable 语义——绝不伪造空结果为「没有 CVE」）；
//   - 单 CVE 匹配链失败 ⇒ 503 如实报错（绝不把失败解释成「该 CVE 无匹配」，
//     B-04C/B-04G 同纪律）；
//   - 排序稳定确定：KEV 优先、CVSS（v3/v4 取大）降序、cve_id 升序兜底；
//   - 预算封顶：单请求 limit（缺省 20、上限 50）、单 CVE 展示资产 ≤10、
//     hinted 点查 ≤16 对/CVE（uq_cve_hints_triple 索引点查，封顶防无界）、
//     整请求 20s 超时——避免 N+1 无界查询。
//
// 租户隔离：路由经 Handler() 的 cors(s.requireAuth(mux)) 注册——JWT 鉴权内
// （本服务为单租户控制台模型，租户隔离=既有鉴权面，零新增暴露）。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/cveintel"
	"github.com/Autumn-27/artex/db"
)

// feed 预算常量（确定性，封顶防无界）。
const (
	cveFeedDefaultLimit    = 20
	cveFeedMaxLimit        = 50
	cveFeedMaxAssetsPerCVE = 10 // 单 CVE 展示资产上限
	cveFeedHintPairBudget  = 16 // 单 CVE hinted 点查预算（索引点查封顶）
	cveFeedTimeout         = 20 * time.Second
)

// CVEFeedAsset 是 feed 里单资产匹配视图（B-04C AssetMatch 的只读投影）。
type CVEFeedAsset struct {
	AssetID        int64   `json:"asset_id"`
	Domain         string  `json:"domain,omitempty"`
	Verdict        string  `json:"verdict"` // affected/maybe（not_affected 已被 B-04C 剔除，绝不复活）
	MatchedProduct string  `json:"matched_product,omitempty"`
	MatchedTech    string  `json:"matched_tech,omitempty"`
	MatchedVersion string  `json:"matched_version,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	TaskIDs        []int64 `json:"task_ids"` // 未终态归属（恒非 nil）
	Hinted         bool    `json:"hinted"`   // 任一未终态任务已有该 CVE 的 hint（台账只读）
}

// CVEFeedItem 是 feed 里单 CVE 聚合视图。
type CVEFeedItem struct {
	CVEID         string         `json:"cve_id"`
	Title         string         `json:"title,omitempty"`
	CVSSv3        *float64       `json:"cvss_v3,omitempty"`
	CVSSv4        *float64       `json:"cvss_v4,omitempty"`
	InKEV         bool           `json:"in_cisa_kev"`
	Published     string         `json:"published,omitempty"`
	MatchedAssets []CVEFeedAsset `json:"matched_assets"` // 恒非 nil（无匹配 = 空数组）
	MatchedTasks  []int64        `json:"matched_tasks"`  // 命中资产任务并集（去重升序，恒非 nil）
	HintedCount   int            `json:"hinted_count"`   // 已有 hint 的 (资产,任务) 对计数
}

// CVEFeedResult 是 feed 响应契约。
type CVEFeedResult struct {
	Days        int           `json:"days"`
	Count       int           `json:"count"`
	Unavailable bool          `json:"unavailable"` // CVE_MCP_URL 未配置或上游不可达（B-01E 显式降级）
	Items       []CVEFeedItem `json:"items"`       // 恒非 nil（无数据 = 合法空列表）
}

// cveFeedHandler 是 GET /api/cve/feed 的处理体（只读；无隐式 hint 写入）。
func (s *Server) cveFeed(w http.ResponseWriter, r *http.Request) {
	days, err := cveFeedDaysArg(r.URL.Query().Get("days"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	limit, err := cveFeedLimitArg(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cveFeedTimeout)
	defer cancel()

	out := CVEFeedResult{Days: days, Items: []CVEFeedItem{}}

	cl, err := cveIntelClient()
	if err != nil {
		if strings.Contains(err.Error(), "CVE_MCP_URL") {
			// 未配置：显式 unavailable（绝不伪造「没有 CVE」）。
			out.Unavailable = true
			writeJSON(w, 200, out)
			return
		}
		writeErr(w, 500, "cve 客户端构造失败")
		return
	}
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 500, "cve feed 存储不可用")
		return
	}

	publishedAfter := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	recs, err := cl.SearchRecent(ctx, publishedAfter, limit)
	if err != nil {
		// 上游不可达：显式 unavailable + 服务端留痕（客户端拿不到原始错误细节）。
		fmt.Fprintf(os.Stderr, "[cve-feed] search 失败（显式 unavailable）: %v\n", err)
		out.Unavailable = true
		writeJSON(w, 200, out)
		return
	}

	for i := range recs {
		rec := &recs[i]
		item, err := s.cveFeedItem(ctx, cl, rec)
		if err != nil {
			// 单 CVE 匹配链失败：如实 503——绝不把失败解释成「无匹配」。
			fmt.Fprintf(os.Stderr, "[cve-feed] %s 匹配链失败: %v\n", rec.CVEID, err)
			writeErr(w, 503, "cve feed 匹配链失败")
			return
		}
		out.Items = append(out.Items, item)
	}
	sortCveFeedItems(out.Items)
	out.Count = len(out.Items)
	writeJSON(w, 200, out)
}

// cveFeedItem 单 CVE 聚合：token/ranges 提取（B-04G 既有纯函数复用）→
// B-04B/B-04C 匹配链（粗筛+精判+未终态归属一次完成）→ hinted 台账只读标记。
func (s *Server) cveFeedItem(ctx context.Context, cl *cveintel.Client, rec *cveintel.Record) (CVEFeedItem, error) {
	item := CVEFeedItem{
		CVEID:         rec.CVEID,
		Title:         rec.Title,
		CVSSv3:        rec.CVSSv3,
		CVSSv4:        rec.CVSSv4,
		InKEV:         rec.InKEV,
		Published:     rec.Published,
		MatchedAssets: []CVEFeedAsset{},
		MatchedTasks:  []int64{},
	}
	tokens := cveTokensFromRecord(rec)
	ranges, eco := cveRangesFromRecord(rec)
	matches, err := s.m.pg.MatchCVEToAssets(ctx, cl, db.CVEMatchInput{
		CVEID:         rec.CVEID,
		Ecosystem:     eco,
		ProductTokens: tokens,
		Ranges:        ranges,
	})
	if err != nil {
		return item, err
	}
	taskSeen := map[int64]bool{}
	hintPairs := 0
	for i, m := range matches {
		if i >= cveFeedMaxAssetsPerCVE {
			break // 展示封顶：匹配集可能大，feed 只读视图不无限展开
		}
		fa := CVEFeedAsset{
			AssetID:        m.AssetID,
			Domain:         m.AssetDomain,
			Verdict:        string(m.Verdict), // db.RangeVerdict → 契约字符串（affected/maybe）
			MatchedProduct: m.MatchedProduct,
			MatchedTech:    m.MatchedTech,
			MatchedVersion: m.MatchedVersion,
			Reason:         m.Reason,
			TaskIDs:        []int64{},
		}
		if m.TaskIDs != nil {
			fa.TaskIDs = m.TaskIDs
		}
		for _, tid := range m.TaskIDs {
			if !taskSeen[tid] {
				taskSeen[tid] = true
				item.MatchedTasks = append(item.MatchedTasks, tid)
			}
			// hinted：cve_hints 台账只读点查（uq_cve_hints_triple 索引；
			// 预算封顶防无界——超预算的对照关闭对视为未 hinted，如实欠报）。
			if hintPairs < cveFeedHintPairBudget {
				hintPairs++
				if hinted, err := s.m.pg.HasCVEHint(tid, m.AssetID, rec.CVEID); err == nil && hinted {
					fa.Hinted = true
					item.HintedCount++
				}
			}
		}
		item.MatchedAssets = append(item.MatchedAssets, fa)
	}
	sort.Slice(item.MatchedTasks, func(a, b int) bool { return item.MatchedTasks[a] < item.MatchedTasks[b] })
	return item, nil
}

// sortCveFeedItems 稳定排序：KEV 优先、CVSS（v3/v4 取大）降序、cve_id 升序
// 兜底（完全确定性）。
func sortCveFeedItems(items []CVEFeedItem) {
	sort.SliceStable(items, func(a, b int) bool {
		x, y := &items[a], &items[b]
		if x.InKEV != y.InKEV {
			return x.InKEV
		}
		if xs, ys := cveFeedTopCVSS(x), cveFeedTopCVSS(y); xs != ys {
			return xs > ys
		}
		return x.CVEID < y.CVEID
	})
}

// ── B-07B：显式 dispatch（用户选定 (CVE, task_ids) 挂 hint）─────────────────
//
// 纪律：只有显式用户操作才写 hint（GET feed 永不自动 dispatch）；写路径只走
// B-04F EmitCVEHints（其内部 AddNodeInScope = 最强授权边 + cve_hints 台账
// 幂等）——绝不直写 exploration_nodes 或 findings；不自动创建任务（建验证
// 任务只能走现有人工建任务接口）；已挂过幂等返回；逐项报告成功/跳过/失败，
// 绝不把局部失败伪装成全部成功。

const (
	// cveFeedDispatchMaxTasks 单次 dispatch 任务数上限（请求体预算）。
	cveFeedDispatchMaxTasks = 20
	// cveFeedDispatchMaxBody 请求体大小上限（不可信输入防御）。
	cveFeedDispatchMaxBody = 64 << 10
)

// cveFeedDispatchRe 是 dispatch 入口的 CVE ID 形态闸（fail-closed）。
var cveFeedDispatchRe = regexp.MustCompile(`^CVE-\d{4}-\d{4,}$`)

// CVEFeedDispatchItem 是单任务 dispatch 结果（逐项报告）。
type CVEFeedDispatchItem struct {
	TaskID    int64  `json:"task_id"`
	Status    string `json:"status"` // emitted|deduped|skipped|throttled|empty|failed
	Emitted   int    `json:"emitted"`
	Deduped   int    `json:"deduped"`
	Throttled int    `json:"throttled"`
	Skipped   int    `json:"skipped"`
	Detail    string `json:"detail,omitempty"` // 失败/跳过原因（脱敏）
}

// CVEFeedDispatchResult 是 dispatch 响应契约。
type CVEFeedDispatchResult struct {
	CVEID string                `json:"cve_id"`
	OK    bool                  `json:"ok"` // 任一任务 failed ⇒ false（局部失败不伪装）
	Items []CVEFeedDispatchItem `json:"items"`
}

// cveFeedDispatchReq 是 dispatch 请求体（用户选定 (CVE, task_ids)）。
type cveFeedDispatchReq struct {
	CVEID   string  `json:"cve_id"`
	TaskIDs []int64 `json:"task_ids"`
}

// cveFeedDispatch 是 POST /api/cve/feed/dispatch 的处理体。
func (s *Server) cveFeedDispatch(w http.ResponseWriter, r *http.Request) {
	var req cveFeedDispatchReq
	body := http.MaxBytesReader(w, r.Body, cveFeedDispatchMaxBody)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体非法（期望 {cve_id, task_ids[]}）")
		return
	}
	cveID := strings.ToUpper(strings.TrimSpace(req.CVEID))
	if !cveFeedDispatchRe.MatchString(cveID) {
		writeErr(w, 400, "cve_id 非法形态（期望 CVE-YYYY-NNNN…）")
		return
	}
	tasks, err := cveFeedDispatchTasks(req.TaskIDs)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 500, "cve feed 存储不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cveFeedTimeout)
	defer cancel()

	out := CVEFeedDispatchResult{CVEID: cveID, OK: true, Items: []CVEFeedDispatchItem{}}

	// 任务预检（任务归属：不存在/已删除的逐项 failed——不整单 400，逐项报告）。
	alive := map[int64]bool{}
	for _, tid := range tasks {
		t, err := s.m.pg.GetTask(tid)
		alive[tid] = err == nil && t != nil
	}

	cl, err := cveIntelClient()
	if err != nil {
		if strings.Contains(err.Error(), "CVE_MCP_URL") {
			writeErr(w, 503, "CVE 服务未配置（CVE_MCP_URL 为空）")
			return
		}
		writeErr(w, 500, "cve 客户端构造失败")
		return
	}
	// 情报记录：not-found ⇒ 404 显式（绝不伪造 hint）；上游失败 ⇒ 503。
	rec, err := cl.Lookup(ctx, cveID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[cve-feed] dispatch %s lookup 失败: %v\n", cveID, err)
		writeErr(w, 503, "cve feed 上游不可用")
		return
	}
	if rec == nil {
		writeErr(w, 404, "CVE 不在本地情报库（先同步或换编号）")
		return
	}
	// 重新匹配（B-04B/B-04C 链）：feed 视图不落库，dispatch 以当前匹配现状
	// 为准——绝不回放陈旧匹配结果。
	ranges, eco := cveRangesFromRecord(rec)
	matches, err := s.m.pg.MatchCVEToAssets(ctx, cl, db.CVEMatchInput{
		CVEID:         cveID,
		Ecosystem:     eco,
		ProductTokens: cveTokensFromRecord(rec),
		Ranges:        ranges,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[cve-feed] dispatch %s 匹配链失败: %v\n", cveID, err)
		writeErr(w, 503, "cve feed 匹配链失败")
		return
	}

	for _, tid := range tasks {
		item := CVEFeedDispatchItem{TaskID: tid, Status: "failed"}
		if !alive[tid] {
			item.Detail = "任务不存在或已删除"
			out.Items = append(out.Items, item)
			out.OK = false
			continue
		}
		// 只保留归属该任务的命中资产（TaskIDs 含 tid）——匹配是资产级全局的，
		// dispatch 是任务级显式操作。
		var sub []db.AssetMatch
		for i := range matches {
			m := matches[i]
			if !containsInt64(m.TaskIDs, tid) {
				continue
			}
			m.TaskIDs = []int64{tid}
			sub = append(sub, m)
		}
		if len(sub) == 0 {
			item.Status = "empty"
			item.Detail = "无命中资产（资产未挂靠该任务或指纹未命中）"
			out.Items = append(out.Items, item)
			continue
		}
		stats, err := s.EmitCVEHints(ctx, sub, rec)
		item.Emitted, item.Deduped, item.Throttled, item.Skipped =
			stats.Emitted, stats.Deduped, stats.Throttled, stats.Skipped
		switch {
		case err != nil:
			item.Status = "failed"
			item.Detail = err.Error()
			out.OK = false
		case stats.Emitted > 0:
			item.Status = "emitted"
		case stats.Deduped > 0:
			item.Status = "deduped"
		case stats.Throttled > 0:
			item.Status = "throttled"
		default:
			item.Status = "skipped"
			item.Detail = "锚点未过任务范围判定或任务无探索图"
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, 200, out)
}

// cveFeedDispatchTasks 校验并归一 task_ids：非空、逐项正数、去重保序、
// 封顶 cveFeedDispatchMaxTasks（fail-closed 400）。
func cveFeedDispatchTasks(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("task_ids 不能为空（显式操作必须指明目标任务）")
	}
	if len(ids) > cveFeedDispatchMaxTasks {
		return nil, fmt.Errorf("task_ids 超上限 %d", cveFeedDispatchMaxTasks)
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("task_ids 含非法值（必须正整数）")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func cveFeedTopCVSS(it *CVEFeedItem) float64 {
	top := -1.0
	for _, v := range []*float64{it.CVSSv3, it.CVSSv4} {
		if v != nil && *v > top {
			top = *v
		}
	}
	return top
}

// cveFeedDaysArg 解析 days：只允许 3/7/30（缺省 7），其他 fail-closed 400。
func cveFeedDaysArg(raw string) (int, error) {
	if raw == "" {
		return 7, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("days 非法（只允许 3/7/30）")
	}
	switch n {
	case 3, 7, 30:
		return n, nil
	}
	return 0, fmt.Errorf("days 非法（只允许 3/7/30）")
}

// cveFeedLimitArg 解析 limit：缺省/0 → 20；负数或超 50 → 400（B-03C 搜索
// 参数同款纪律：不静默纠偏）。
func cveFeedLimitArg(raw string) (int, error) {
	if raw == "" {
		return cveFeedDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("limit 非法（0-%d，缺省 %d）", cveFeedMaxLimit, cveFeedDefaultLimit)
	}
	if n == 0 {
		return cveFeedDefaultLimit, nil
	}
	if n > cveFeedMaxLimit {
		return 0, fmt.Errorf("limit 超上限 %d", cveFeedMaxLimit)
	}
	return n, nil
}
