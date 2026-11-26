package server

// CVE 情报只读工具面（B-03G；B-01E 降级语义在此落地）。
//
// 与 biz_tools.go / edge_tools.go 同一通道：server 侧参数化工具 + hostTools 装配
// + tools 表按 agent 绑定 + 角色副闸（agent/roledispatch.go）。四工具经 cveintel
// 客户端（B-01D 只读适配器，端点来自 CVE_MCP_URL 运维配置）调用 sidecar 的
// cve_intel.v1 只读面——绝不绕过安全管道直连：进 hostTools 的工具在
// agent/assembly.go:107 统一装饰 enforceToolPolicy → guardPanic → SoD 执行器
// （未登记名默认受管），装配后过 ToolResolve（tools 表绑定过滤 + meterTool 用量
// 台账）与 filterToolsForRole（角色白名单副闸）。
//
// 纪律（B 阶段包安全要求逐条对应）：
//   - 只读：lookup / search / match / sync-status 四个只读能力；同步触发工具
//     （sync-once / 手动 sync）绝不进 Agent 工具面。
//   - 情报只是 hint：结果绝不直接形成 finding、不授权利用、不扩大任务 scope
//     （工具描述里对模型明说）。
//   - B-01E 三态区分：未配置（CVE_MCP_URL 为空）/ 服务不可达（连接失败、超时、
//     5xx）→ 显式 status:"unavailable"，绝不伪装成「没有漏洞」（不伪造空列表、
//     不伪造 found:false）；not-found（查无此 CVE）是另一回事，显式 status:
//     "not_found"；空查询结果显式 status:"ok" + 空数组。工具异常只影响本次调用
//     返回值，不污染任务状态。
//   - 最小 scope：默认零 agent 绑定（零行为变化，UI 按需勾绑）；超时走 cveintel
//     客户端缺省 5s；零凭据头（本机只读面）；地址只用运维配置的 CVE_MCP_URL，
//     不接受任意 URL。
//   - 延迟披露（延迟披露卡目标）：四工具对绑定 agent 走 deferred 通道——schema
//     不进系统提示、名字进 <available-deferred-tools> 块、经 SearchExtraTools/
//     ExecuteExtraTool 按需披露（hostTools 的 deferredBinds 映射，代码常量，
//     与 roledispatch 同款不受 DB 改动影响）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/cveintel"
)

// errCveNotConfigured 是「未配置」与「不可达」分开的语义锚（B-01E 要求 ②）。
var errCveNotConfigured = errors.New("CVE 服务未配置（CVE_MCP_URL 为空）")

// newCveIntelClient 是客户端构造钩子（测试注入短超时客户端用；生产即 cveintel.New）。
var newCveIntelClient = func(base string) (*cveintel.Client, error) { return cveintel.New(base) }

// cveIntelClient 每次调用现构造客户端：无状态（零连接缓存），构造即完成端点
// SSRF 校验；CVE_MCP_URL 在进程启动期由部署注入（B-01F start.sh / compose），
// 运行期不变 ⇒ 不需要缓存。空值 = 未配置 → 调用方走显式 unavailable，绝不回退
// 写死地址。
func cveIntelClient() (*cveintel.Client, error) {
	base := cveintel.URLFromEnv()
	if base == "" {
		return nil, errCveNotConfigured
	}
	cl, err := newCveIntelClient(base)
	if err != nil {
		return nil, fmt.Errorf("CVE 客户端构造失败: %w", err)
	}
	return cl, nil
}

// cveToolNames 是四工具名（deferred 披露与种子都以它为准；顺序即装配序）。
var cveToolNames = []string{"cve_lookup", "cve_search", "cve_match_component", "cve_sync_status"}

// cveTools 返回 CVE 只读工具全集（进 hostTools 装配面）。
func (s *Server) cveTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCveLookup(),
		s.toolCveSearch(),
		s.toolCveMatchComponent(),
		s.toolCveSyncStatus(),
	}
}

// seedCveTools 播种四工具行（首插生效，UI 编辑不被覆盖）。默认【零绑定】：最小
// scope——没有任何 agent 默认可见，操作者在工具页按需勾绑（零行为变化）。绑定
// 后仍走 deferred 披露（cveDeferredBinds），schema 不直接进提示词。
func (s *Server) seedCveTools() {
	for _, t := range s.cveTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, json.RawMessage("[]"))
	}
	agent.InvalidateAssembly()
}

// cveDeferredBinds 读四工具的延迟披露绑定：行存在且 Enabled 才披露，绑定数组
// 来自 tools 表（谁绑了谁能在 deferred 块里看到名字）。deferred 语义是代码常量
// （不依赖行上可被 UI 翻转的 deferred 列）——「情报工具按需披露」是本卡设计
// 决定，与 roledispatch 的常量白名单同一纪律。
func (s *Server) cveDeferredBinds() map[string][]string {
	out := map[string][]string{}
	rows, err := s.m.pg.ListTools()
	if err != nil {
		return out // 读失败按未披露处理（fail-closed：少披露不误披露）
	}
	want := map[string]bool{}
	for _, n := range cveToolNames {
		want[n] = true
	}
	for _, r := range rows {
		if want[r.Key] && r.Enabled {
			out[r.Key] = r.Agents
		}
	}
	return out
}

// ---------- 结果语义（B-01E） ----------

// cveSingleLine 把错误文本压成单行并截断——客户端错误文本本身零凭据（cveintel
// 契约：零凭据头、零 body 回显），这里再兜一层长度/换行，日志与工具结果都干净。
func cveSingleLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// cveUnavailableResult 显式 unavailable：模型可读的结构化状态，绝不是伪造的
// 空结果（B-01E 纪律：不可达 ≠ 没有漏洞）。
func cveUnavailableResult(err error) (actool.Result, error) {
	return jsonResult(map[string]any{
		"status": "unavailable",
		"hint":   "CVE 情报服务当前不可用；本结果是服务状态，不代表目标没有漏洞。不要把它解释为 not_affected。",
		"error":  cveSingleLine(err.Error()),
	})
}

// cveResultFromErr 把 cveintel 有类型错误映射成显式工具结果：Unavailable →
// unavailable；Protocol/TooLarge/Incompatible → protocol_error（同样显式，绝不
// 静默吞成空）。
func cveResultFromErr(err error) (actool.Result, error) {
	switch {
	case errors.Is(err, cveintel.ErrUnavailable):
		return cveUnavailableResult(err)
	case errors.Is(err, cveintel.ErrProtocol), errors.Is(err, cveintel.ErrTooLarge), errors.Is(err, cveintel.ErrIncompatible):
		return jsonResult(map[string]any{
			"status": "protocol_error",
			"hint":   "CVE 服务响应不可信或契约不兼容；本结果不代表目标没有漏洞。",
			"error":  cveSingleLine(err.Error()),
		})
	default:
		return actool.Errorf(cveSingleLine(err.Error())), nil
	}
}

// ---------- 参数助手 ----------

// cveLimitArg 解析 search 的 limit（缺省 20；数字或数字字符串；1-100 之外显式
// 参数错误——与 sidecar B-03C 同纪律：垃圾值不静默纠偏）。
func cveLimitArg(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 20, nil
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		if n >= 1 && n <= 100 {
			return n, nil
		}
		return 0, fmt.Errorf("limit 必须为 1-100，得到 %d", n)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if n, v := parseLimitInt(str); v == nil && n >= 1 && n <= 100 {
			return n, nil
		}
		return 0, fmt.Errorf("limit 必须为 1-100（数字），得到 %q", string(raw))
	}
	return 0, fmt.Errorf("limit 形态非法: %s", string(raw))
}

func parseLimitInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空串")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0, fmt.Errorf("过大")
		}
	}
	return n, nil
}

// cveStrArg 取必填字符串参数（去空白；空白 = 参数错误）。
func cveStrArg(in map[string]json.RawMessage, key string) (string, error) {
	raw, ok := in[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return "", fmt.Errorf("%s 为必填", key)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%s 必须为字符串", key)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s 为必填", key)
	}
	return v, nil
}

// cveOptStrArg 取可选字符串参数（缺省/空白 → 空串）。
func cveOptStrArg(in map[string]json.RawMessage, key string) string {
	raw, ok := in[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v string
	_ = json.Unmarshal(raw, &v)
	return strings.TrimSpace(v)
}

// ---------- 工具 ----------

// toolCveLookup 按 CVE id 精确查（sidecar cve_lookup 同名同义）。
func (s *Server) toolCveLookup() actool.CoreTool {
	return roTool("cve_lookup",
		"按 CVE 编号查官方漏洞情报（NVD/CISA KEV/GitHub Advisory 聚合，只读）。返回 cve_intel.v1 记录：CVSS、CWE、是否在 KEV、受影响版本区间、修复版本、参考链接与相关审计技能。★情报只是提示（hint）：它帮助你在测试中判断「值不值得看」，绝不能仅凭它直接判定漏洞存在或不存在，登记漏洞仍须以实测证据走 report_finding。结果带 status：ok=命中；not_found=查无此编号（≠没有漏洞）；unavailable=服务不可用（≠没有漏洞）；protocol_error=响应不可信。",
		objSchema(map[string]any{
			"cve_id": strParam("CVE 编号，如 CVE-2021-44228"),
		}, "cve_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(in, &m)
			id, err := cveStrArg(m, "cve_id")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cl, err := cveIntelClient()
			if err != nil {
				return cveUnavailableResult(err)
			}
			rec, err := cl.Lookup(ctx, id)
			if err != nil {
				return cveResultFromErr(err)
			}
			if rec == nil {
				// not-found ≠ unavailable（B-03B 同源语义），显式区分。
				return jsonResult(map[string]any{"status": "not_found", "cve_id": strings.ToUpper(id)})
			}
			return jsonResult(map[string]any{"status": "ok", "record": rec})
		})
}

// toolCveSearch 全文检索 CVE 情报（sidecar cve_search 同名同义，limit 1-100 缺省 20）。
func (s *Server) toolCveSearch() actool.CoreTool {
	return roTool("cve_search",
		"按关键字检索 CVE 情报库（只读；命中标题/描述/CWE/产品等字段）。用于给测试方向找候选：比如目标用了 log4j，先搜一圈看有哪些相关 CVE 值得核对版本。★情报只是提示（hint）：搜到候选≠目标受影响，务必用 cve_match_component 核对版本区间或以实测为准。limit 可选（1-100，缺省 20）。结果带 status：ok（records 可为空数组=真没有匹配）；unavailable=服务不可用（≠没有匹配）。",
		objSchema(map[string]any{
			"query": strParam("关键字（产品名/CVE 编号片段/技术词）"),
			"limit": map[string]any{"type": "integer", "description": "可选：返回条数上限，1-100，缺省 20"},
		}, "query"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(in, &m)
			q, err := cveStrArg(m, "query")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			limit, err := cveLimitArg(m["limit"])
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cl, err := cveIntelClient()
			if err != nil {
				return cveUnavailableResult(err)
			}
			recs, err := cl.Search(ctx, q, limit)
			if err != nil {
				return cveResultFromErr(err)
			}
			return jsonResult(map[string]any{"status": "ok", "query": q, "records": recs})
		})
}

// toolCveMatchComponent 组件×CVE 三态匹配（sidecar cve_match_component 同名同义，
// B-05 判定：affected / not_affected / maybe——不确定一律 maybe，绝不假阴性）。
func (s *Server) toolCveMatchComponent() actool.CoreTool {
	return roTool("cve_match_component",
		"判断某组件版本是否落在已知 CVE 的受影响区间（只读；三态判定 affected/not_affected/maybe）。传 package+ecosystem+version 匹配情报库；或传 ranges（OSV events 或 from/to/fixed 形态）做纯区间求值。★三态纪律：maybe=版本无法比较或区间缺失，绝不能读成 not_affected；只有实测确认才能登记漏洞。结果带 status：ok（verdict 三态之一）；unavailable=服务不可用。",
		objSchema(map[string]any{
			"package_or_cpe": strParam("包名或 CPE（如 log4j-core / maven:org.apache.logging.log4j:log4j-core）"),
			"ecosystem":      map[string]any{"type": "string", "description": "可选：生态（maven/npm/pypi/go…）；缺省按库内记录匹配"},
			"version":        map[string]any{"type": "string", "description": "组件版本；缺省=只做包名粗筛（verdict 多为 maybe）"},
			"cve_id":         map[string]any{"type": "string", "description": "可选：限定单个 CVE"},
			"ranges":         map[string]any{"type": "array", "description": "可选：调用方区间纯求值（不查库），元素形态 {events:[{introduced|fixed|last_affected:ver}]} 或 {from,to,fixed,to_inclusive}"},
		}, "package_or_cpe"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(in, &m)
			pkg, err := cveStrArg(m, "package_or_cpe")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			eco := cveOptStrArg(m, "ecosystem")
			ver := cveOptStrArg(m, "version")
			cveID := cveOptStrArg(m, "cve_id")
			cl, err := cveIntelClient()
			if err != nil {
				return cveUnavailableResult(err)
			}
			if raw, has := m["ranges"]; has && len(raw) > 0 && string(raw) != "null" {
				var ranges []cveintel.Range
				if err := json.Unmarshal(raw, &ranges); err != nil {
					return actool.Errorf("ranges 形态非法（期望 {events:[…]} 或 {from,to,fixed,…} 数组）: " + cveSingleLine(err.Error())), nil
				}
				verdict, reason, err := cl.EvaluateRanges(ctx, eco, ver, ranges)
				if err != nil {
					return cveResultFromErr(err)
				}
				return jsonResult(map[string]any{"status": "ok", "verdict": verdict, "reason": reason})
			}
			items, err := cl.MatchComponent(ctx, eco, pkg, ver)
			if err != nil {
				return cveResultFromErr(err)
			}
			if cveID != "" {
				items = filterMatchItems(items, cveID)
			}
			return jsonResult(map[string]any{"status": "ok", "items": items})
		})
}

// filterMatchItems 按 cve_id 前缀过滤匹配项（cve_id 参数的可选限定语义）。
func filterMatchItems(items []cveintel.MatchItem, cveID string) []cveintel.MatchItem {
	cveID = strings.ToUpper(strings.TrimSpace(cveID))
	out := items[:0:0]
	for _, it := range items {
		if it.Cve != nil && strings.EqualFold(it.Cve.CVEID, cveID) {
			out = append(out, it)
		}
	}
	return out
}

// toolCveSyncStatus 读同步新鲜度（sidecar cve_sync_status 同名同义；B-03E 只读
// 契约——查询永不触发同步）。这是「status」只读能力，不是 sync 工具：同步触发
// （sync-once / 手动 sync）不在 Agent 工具面。
func (s *Server) toolCveSyncStatus() actool.CoreTool {
	return roTool("cve_sync_status",
		"读 CVE 情报库的同步状态（只读；kev/nvd/ghsa 三源各自的新鲜度、记录数、最后成功/失败时间）。用于判断情报可信度：stale 或 unknown 的源说明数据可能过时，匹配结果要打折看待。绝不触发同步。结果带 status：ok（sources 三源明细）；unavailable=服务不可用。",
		objSchema(map[string]any{}),
		func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
			cl, err := cveIntelClient()
			if err != nil {
				return cveUnavailableResult(err)
			}
			st, err := cl.SyncStatus(ctx)
			if err != nil {
				return cveResultFromErr(err)
			}
			return jsonResult(map[string]any{"status": "ok", "sync": st})
		})
}
