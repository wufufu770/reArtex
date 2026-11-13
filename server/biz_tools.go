package server

// 业务建模工具面（BIZ 合并施工 M3/M5/M7）。
//
// 设计要点（为什么是 server 侧参数化工具而不是 ToolSet 任务工具）：
//   - chat lane 没有任务级 ToolSet（buildDomainReg 的 nil ExplorationStore 是
//     deliberate，见 server/assembly.go）——bizmodeler 是会话型自定义 agent，
//     它的工具必须自带 company_id/task_id 参数、由工具内部取数与判范围。
//   - 写工具的授权语义在 db 层强制（UpsertBizNodeInScope 等：强制锚点 + 范围
//     判定前置 + 越界整单拒），这里只做参数校验与转调——闸门不重复实现。
//   - 派生（propose_biz_intent）是两段式的第二段：先 fact 后 intent 的硬约束
//     由探索图 addOneIntent 的 parent 校验承担，这里做的是业务侧的额外纪律：
//     归一去重（NormalizeIntentSummary）、积压水位感知（T5）、可证伪预测必填
//     （F1——没有预测的意图会被基座自己的许愿纪律记为矛盾，建模派生的意图
//     必须自带「期待观察到什么」）。
//   - 全部工具走 hostTools 通道进每个 agent 的装配面，再由 tools 表按绑定过滤
//     （默认只绑 bizmodeler）；roledispatch 的 bizModelerToolAllow 双闸兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// bizTools 返回业务建模工具全集（进 hostTools 装配面，tools 表按绑定过滤）。
func (s *Server) bizTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolReadBizGraph(),
		s.toolReadDesignArtifacts(),
		s.toolUpsertBizNode(),
		s.toolUpsertBizEdge(),
		s.toolRecordBizDiff(),
		s.toolProposeBizIntent(),
		s.toolBizRefutedFacts(),
		s.toolBizAdjudicate(),
	}
}

// seedBizTools 把业务建模工具播种进 tools 表（首插生效，DO NOTHING 保留用户编辑）
// 并写默认绑定。refresh 旗标照 refreshBuiltinToolSchemas 惯例：schema 变更时 bump
// 一次性旗标刷新描述与 schema（保留绑定与 enabled）。收尾 InvalidateAssembly：
// G1 装配缓存的 fresh() 探针不含 tools 表，直写 DB 后必须翻代。
func (s *Server) seedBizTools() {
	const refreshFlag = "biz_tools_seed_v1"
	bindings, _ := json.Marshal([]string{"bizmodeler"})
	for _, t := range s.bizTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	if v, _, _ := s.m.pg.GetSetting(refreshFlag); v != "true" {
		_ = s.m.pg.SetSetting(refreshFlag, "true")
	}
	agent.InvalidateAssembly()
}

// bizCtxFromTaskID 从工具入参取 company_id 与 task_id（写入路径双必填）。
type bizCtx struct {
	CompanyID int64
	TaskID    int64
}

func parseBizCtx(in json.RawMessage) (*bizCtx, error) {
	var a struct {
		CompanyID int64 `json:"company_id"`
		TaskID    int64 `json:"task_id"`
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return nil, err
	}
	if a.CompanyID <= 0 {
		return nil, fmt.Errorf("company_id 为必填（业务节点按公司归属）")
	}
	if a.TaskID <= 0 {
		return nil, fmt.Errorf("task_id 为必填：业务图写入必须有任务上下文（没有授权边就不落笔，与探索图锚点同一 fail-closed 语义）")
	}
	return &bizCtx{CompanyID: a.CompanyID, TaskID: a.TaskID}, nil
}

// toolReadBizGraph 按公司/层/类型读业务图（调用者范围过滤在 db 层）。
func (s *Server) toolReadBizGraph() actool.CoreTool {
	return roTool("read_biz_graph",
		"读业务建模图（第三张图）：实体/动作/状态/角色/信任边界/数据流/外部系统节点与其关系，分 design（系统声称）与 behavior（系统实际）两层。只返回调用者范围内的部分——锚点落在范围外的节点整体不可见（范围读的授权边语义）。task_id 不传时按平台范围读（未配置平台范围则什么都看不到，不回退全局）。",
		objSchema(map[string]any{
			"company_id": map[string]any{"type": "integer", "description": "公司 id（必填）"},
			"task_id":    map[string]any{"type": "integer", "description": "任务 id（可选）：传了按该任务的范围读；不传按平台范围"},
			"layer":      strParam("层过滤：design|behavior（可选，空=两层都返回）"),
			"node_type":  strParam("节点类型过滤：biz_entity|biz_action|biz_state|biz_role|biz_trust|biz_dataflow|biz_external（可选）"),
			"limit":      map[string]any{"type": "integer", "description": "节点上限，默认 1000"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				CompanyID int64  `json:"company_id"`
				TaskID    int64  `json:"task_id"`
				Layer     string `json:"layer"`
				NodeType  string `json:"node_type"`
				Limit     int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if a.CompanyID <= 0 {
				return actool.Errorf("company_id 为必填"), nil
			}
			g, err := s.m.pg.ListBizGraphScoped(a.TaskID, a.CompanyID, a.Layer, a.NodeType, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if g.Nodes == nil {
				g.Nodes = []*db.BizNode{}
			}
			if g.Edges == nil {
				g.Edges = []*db.BizEdge{}
			}
			return jsonResult(g)
		})
}

// designArtifactNames 是 api-recon Phase 4 的已知产物名（OUTDIR 产物，skill 侧
// 约定写进共享工作目录）。read_design_artifacts 默认按这些名字扫描。
var designArtifactNames = []string{
	"userRouteAuth.json", "permissions_tree.json", "route_map.json",
	"perm_codes_all.txt", "routes.txt", "api_static.txt",
}

// miniprogramArtifactNames 是小程序解包还原工程里的设计层文件（0.12.0 §四-B4）：
// app.json=页面注册树（pages/entryPagePath/window 配置），project.config.json=
// 编译开关（五关两开的矫正对象）。扫描 artifacts/miniprogram/<appid>/。
var miniprogramArtifactNames = []string{
	"app.json", "project.config.json",
}

// toolReadDesignArtifacts 读共享工作目录里的前端授权产物并解析成结构化摘要
// （角色乘路由矩阵 / 权限码树 / 路由表）。只读，不改脚本、不跑脚本——解析在
// 服务端做，模型不经 Read 工具碰宿主 FS（bizModelerToolAllow 收敛为零接触面）。
func (s *Server) toolReadDesignArtifacts() actool.CoreTool {
	return roTool("read_design_artifacts",
		"读设计层证据：共享工作目录里 api-recon 的权限树/路由表产物（userRouteAuth.json=角色×路由授权矩阵、permissions_tree.json=权限码树、route_map.json=路由映射、perm_codes_all.txt=权限码全集、routes.txt/api_static.txt=静态路由清单）。paths 传产物所在目录（相对共享工作目录，如 recon）；不传则在工作目录浅层扫描已知产物名。返回解析后的结构化摘要，供建模的实体抽取与权限视图。",
		objSchema(map[string]any{
			"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "候选目录/文件（相对共享工作目录，如 [\"recon\",\"recon/js\"]）；可选"},
			"limit": map[string]any{"type": "integer", "description": "每份产物的摘要条数上限，默认 50"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Paths []string `json:"paths"`
				Limit int      `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 50
			}
			// 收集候选目录：显式 paths + 工作目录根（浅层扫描）。
			dirs := []string{"."}
			for _, p := range a.Paths {
				dirs = append(dirs, p)
			}
			found := map[string]string{} // artifact 名 → 相对路径
			for _, dir := range dirs {
				abs, ok := s.wsResolve(dir)
				if !ok {
					continue
				}
				entries, err := os.ReadDir(abs)
				if err != nil {
					continue
				}
				for _, e := range entries {
					for _, name := range designArtifactNames {
						if _, dup := found[name]; dup {
							continue
						}
						if e.Name() == name {
							found[name] = filepath.Join(dir, e.Name())
						}
					}
				}
			}
			// 小程序产物（0.12.0 §四-B4）：artifacts/miniprogram/<appid>/ 下解包还原
			// 的源码工程是设计层的直接来源（app.json=页面注册树，project.config.json=
			// 编译开关）。key 用 name@appid 区分多应用；摘要按基名走同一解析器。
			if mpBase, mpOK := s.wsResolve(mpArtifactRootRel); mpOK {
				if subs, err := os.ReadDir(mpBase); err == nil {
					for _, sub := range subs {
						if !sub.IsDir() {
							continue
						}
						for _, name := range miniprogramArtifactNames {
							key := name + "@" + sub.Name()
							if _, dup := found[key]; dup {
								continue
							}
							if _, err := os.Stat(filepath.Join(mpBase, sub.Name(), name)); err == nil {
								found[key] = mpArtifactRootRel + "/" + sub.Name() + "/" + name
							}
						}
					}
				}
			}
			if len(found) == 0 {
				return actool.Errorf("共享工作目录里没有找到设计层产物（api-recon：" + strings.Join(designArtifactNames, " / ") + "；小程序：artifacts/miniprogram/<appid>/ 下的 " + strings.Join(miniprogramArtifactNames, " / ") + "）。先对该目标跑 skills/api-recon（Phase 4 会产出权限树）或 skills/wechat-miniprogram（解包还原），或用 paths 参数指定产物目录。"), nil
			}
			out := map[string]any{"artifacts": map[string]any{}}
			arts := out["artifacts"].(map[string]any)
			names := make([]string, 0, len(found))
			for name := range found {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				rel := found[name]
				abs, ok := s.wsResolve(rel)
				if !ok {
					continue
				}
				raw, err := os.ReadFile(abs)
				if err != nil {
					arts[name] = map[string]any{"path": rel, "error": err.Error()}
					continue
				}
				base := name
				if idx := strings.Index(base, "@"); idx >= 0 {
					base = name[:idx]
				}
				arts[name] = summarizeDesignArtifact(base, raw, a.Limit)
				arts[name+".path"] = rel
			}
			return jsonResult(out)
		})
}

// summarizeDesignArtifact 把一份产物解析成结构化摘要（纯 Go，零 LLM）。
func summarizeDesignArtifact(name string, raw []byte, limit int) any {
	switch {
	case name == "userRouteAuth.json":
		// 角色乘路由授权矩阵：{role: {code: url}} 或 {role: [routes]} 两种常见形状都容。
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return map[string]any{"error": "解析失败: " + err.Error()}
		}
		matrix := map[string]any{}
		total := 0
		for role, v := range m {
			routes := map[string]any{}
			switch t := v.(type) {
			case map[string]any:
				for code, url := range t {
					routes[code] = url
					total++
				}
			case []any:
				for _, r := range t {
					key := fmt.Sprintf("%v", r)
					routes[key] = key
					total++
				}
			}
			matrix[role] = routes
		}
		return map[string]any{"kind": "role×route matrix", "roles": len(matrix), "entries_total": total, "matrix": clipMatrix(matrix, limit)}
	case name == "permissions_tree.json":
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return map[string]any{"error": "解析失败: " + err.Error()}
		}
		return map[string]any{"kind": "permission tree", "tree_preview": previewJSON(tree, limit)}
	case name == "route_map.json":
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return map[string]any{"error": "解析失败: " + err.Error()}
		}
		return map[string]any{"kind": "route map", "routes": len(m), "preview": previewJSON(m, limit)}
	case name == "app.json":
		// 小程序页面注册树：pages 数组 + entryPagePath + window 配置（设计层的
		// 页面注册来源；实体抽取用它枚举界面入口）。
		var m struct {
			Pages         []string       `json:"pages"`
			EntryPagePath string         `json:"entryPagePath"`
			Window        map[string]any `json:"window"`
			SubPackages   []struct {
				Root  string   `json:"root"`
				Pages []string `json:"pages"`
			} `json:"subPackages"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return map[string]any{"error": "解析失败: " + err.Error()}
		}
		pages := m.Pages
		if len(pages) > limit {
			pages = pages[:limit]
		}
		subTotals := 0
		for _, sp := range m.SubPackages {
			subTotals += len(sp.Pages)
		}
		return map[string]any{"kind": "miniprogram page registry", "main_pages": len(m.Pages),
			"pages_preview": pages, "entry": m.EntryPagePath,
			"subpackages": len(m.SubPackages), "subpackage_pages": subTotals,
			"window": m.Window}
	case name == "project.config.json":
		// 编译开关（五关两开的矫正对象）：矫正脚本跑完后这里应显示全关。
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return map[string]any{"error": "解析失败: " + err.Error()}
		}
		setting, _ := m["setting"].(map[string]any)
		return map[string]any{"kind": "compile switches", "setting": setting,
			"compileType": m["compileType"], "libVersion": m["libVersion"]}
	default:
		// 文本产物（perm_codes_all.txt / routes.txt / api_static.txt）：行数 + 前 N 行。
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		head := lines
		if len(head) > limit {
			head = head[:limit]
		}
		return map[string]any{"kind": "text lines", "total_lines": len(lines), "head": head}
	}
}

func clipMatrix(m map[string]any, limit int) map[string]any {
	out := map[string]any{}
	for role, v := range m {
		if mm, ok := v.(map[string]any); ok {
			if len(mm) > limit {
				clipped := map[string]any{}
				keys := make([]string, 0, len(mm))
				for k := range mm {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys[:limit] {
					clipped[k] = mm[k]
				}
				clipped["..."] = fmt.Sprintf("（共 %d 条，已截断）", len(mm))
				out[role] = clipped
				continue
			}
		}
		out[role] = v
	}
	return out
}

func previewJSON(v any, limit int) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	s := string(b)
	if len(s) > limit*200 {
		s = s[:limit*200] + " …（已截断）"
	}
	return json.RawMessage(s)
}

// toolUpsertBizNode 写业务节点（强制锚点+范围判定在 db 层）。
func (s *Server) toolUpsertBizNode() actool.CoreTool {
	return wrTool("upsert_biz_node",
		"写一个业务建模节点。★必须先有依据：props.fact_id 填依据的探索图 fact 节点 id（没有 fact 依据的猜测不许落图）；asset_ids 必须传该节点接触的资产 id（强制锚点——不锚资产的节点不写）。同名同层同类的节点只留一行（重复建模是更新不是新增）。",
		objSchema(map[string]any{
			"company_id": map[string]any{"type": "integer", "description": "公司 id（必填）"},
			"task_id":    map[string]any{"type": "integer", "description": "任务 id（必填）：无任务上下文整单拒绝"},
			"layer":      strParam("design（系统声称）| behavior（系统实际）——必填"),
			"node_type":  strParam("biz_entity|biz_action|biz_state|biz_role|biz_trust|biz_dataflow|biz_external——必填"),
			"name":       strParam("规范化名称（必填）；名字不明先记 raw_name"),
			"raw_name":   strParam("原始出处名（路由名/权限码/接口名）"),
			"description": strParam("一句话说明"),
			"props":      map[string]any{"type": "object", "description": "自由属性；约定键 fact_id（依据的 fact 节点 id）、route、code 等"},
			"confidence": strParam("observed（输出里直接看到）| inferred（据现象推断），默认 inferred——与探索图 fact 同一口径"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "锚定资产 id（必填，≥1 个；list_assets 返回的资产 id）"},
		}, "company_id", "task_id", "layer", "node_type", "name"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				CompanyID   int64          `json:"company_id"`
				TaskID      int64          `json:"task_id"`
				Layer       string         `json:"layer"`
				NodeType    string         `json:"node_type"`
				Name        string         `json:"name"`
				RawName     string         `json:"raw_name"`
				Description string         `json:"description"`
				Props       map[string]any `json:"props"`
				Confidence  string         `json:"confidence"`
				AssetIDs    []int64        `json:"asset_ids"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Layer != db.BizLayerDesign && a.Layer != db.BizLayerBehavior {
				return actool.Errorf("layer 必须是 design 或 behavior"), nil
			}
			switch a.NodeType {
			case db.BizNodeEntity, db.BizNodeAction, db.BizNodeState, db.BizNodeRole,
				db.BizNodeTrust, db.BizNodeDataflow, db.BizNodeExternal:
			default:
				return actool.Errorf("node_type 必须是七类之一（biz_entity/biz_action/biz_state/biz_role/biz_trust/biz_dataflow/biz_external）"), nil
			}
			id, err := s.m.pg.UpsertBizNodeInScope(a.TaskID, a.CompanyID, db.BizNodeInput{
				Layer: a.Layer, NodeType: a.NodeType, Name: a.Name, RawName: a.RawName,
				Description: a.Description, Props: a.Props, Confidence: a.Confidence, Anchors: a.AssetIDs,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("biz node #%d upserted（layer=%s type=%s name=%s）", id, a.Layer, a.NodeType, a.Name)), nil
		})
}

// toolUpsertBizEdge 写业务边（两端锚点范围判定在 db 层）。
func (s *Server) toolUpsertBizEdge() actool.CoreTool {
	return wrTool("upsert_biz_edge",
		"写一条业务建模边（owns/invokes/depends_on/trusts/flows_to/validates/requires_state/transitions/grants）。两端节点 id 用 upsert_biz_node 返回的 id。强度高于 inferred 的关系必须附一条反事实追问（props.counterfactual：若此关系不成立，会观察到什么？）。grants 表达「角色×资源×操作」的授予——权限矩阵靠它把角色和动作连起来。",
		objSchema(map[string]any{
			"company_id": map[string]any{"type": "integer", "description": "公司 id（必填）"},
			"task_id":    map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"layer":      strParam("design|behavior——必填"),
			"src_id":     map[string]any{"type": "integer", "description": "起点业务节点 id（必填）"},
			"dst_id":     map[string]any{"type": "integer", "description": "终点业务节点 id（必填）"},
			"rel":        strParam("owns|invokes|depends_on|trusts|flows_to|validates|requires_state|transitions|grants——必填"),
			"props":      map[string]any{"type": "object", "description": "自由属性；约定键 counterfactual（反事实追问）"},
			"confidence": strParam("observed|inferred，默认 inferred"),
		}, "company_id", "task_id", "layer", "src_id", "dst_id", "rel"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				CompanyID  int64          `json:"company_id"`
				TaskID     int64          `json:"task_id"`
				Layer      string         `json:"layer"`
				SrcID      int64          `json:"src_id"`
				DstID      int64          `json:"dst_id"`
				Rel        string         `json:"rel"`
				Props      map[string]any `json:"props"`
				Confidence string         `json:"confidence"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Layer != db.BizLayerDesign && a.Layer != db.BizLayerBehavior {
				return actool.Errorf("layer 必须是 design 或 behavior"), nil
			}
			id, err := s.m.pg.UpsertBizEdgeInScope(a.TaskID, a.CompanyID, db.BizEdgeInput{
				Layer: a.Layer, SrcID: a.SrcID, DstID: a.DstID, Rel: a.Rel,
				Props: a.Props, Confidence: a.Confidence,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("biz edge #%d upserted（%d -%s-> %d）", id, a.SrcID, a.Rel, a.DstID)), nil
		})
}

// toolRecordBizDiff 登记双层差异（WSTG/STRIDE/severity 由规则表补全，模型只给类型）。
func (s *Server) toolRecordBizDiff() actool.CoreTool {
	return wrTool("record_biz_diff",
		"登记一条 design 与 behavior 的双层差异。diff_type 八选一：missing_validation（声明了校验但行为没有）/ extra_permission（行为出现未声明的权限）/ state_bypass（状态转移被跳过）/ trust_violation（信任链断点）/ unexpected_flow（意外数据流）/ timing_anomaly（时序冲突）/ unbounded_consumption（额度未受限）/ workflow_skip（孤立/跳步动作）。severity 与 WSTG/STRIDE 编号由规则表自动补全，不用你给。design_ref/behavior_ref 里可带 node_ids（引用业务节点 id）。",
		objSchema(map[string]any{
			"company_id":   map[string]any{"type": "integer", "description": "公司 id（必填）"},
			"task_id":      map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"diff_type":    strParam("八类之一——必填"),
			"description":  strParam("差异说明：design 层声称什么、behavior 层实际观察到什么、依据哪些 fact"),
			"design_ref":   map[string]any{"type": "object", "description": "设计层证据（node_ids/路由/权限码等）"},
			"behavior_ref": map[string]any{"type": "object", "description": "行为层证据（node_ids/fact_id/资产 id 等）"},
		}, "company_id", "task_id", "diff_type", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				CompanyID   int64          `json:"company_id"`
				TaskID      int64          `json:"task_id"`
				DiffType    string         `json:"diff_type"`
				Description string         `json:"description"`
				DesignRef   map[string]any `json:"design_ref"`
				BehaviorRef map[string]any `json:"behavior_ref"`
			}
			_ = json.Unmarshal(in, &a)
			id, err := s.m.pg.RecordBizDiffInScope(a.TaskID, a.CompanyID, a.DiffType, a.Description, a.DesignRef, a.BehaviorRef)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			rule := db.BizDiffRules[a.DiffType]
			return actool.Text(fmt.Sprintf("biz diff #%d recorded（%s severity=%s %s/%s）。高价值差异可用 propose_biz_intent 派生成探索意图。", id, a.DiffType, rule.Severity, rule.WSTG, rule.STRIDE)), nil
		})
}

// toolProposeBizIntent 把一条业务差异/假设派生成探索图意图（两段式第二段）。
func (s *Server) toolProposeBizIntent() actool.CoreTool {
	return wrTool("propose_biz_intent",
		"把一条业务假设派生成探索图 intent（它会被 worker 领走验证）。★硬前置：parent_fact_id 必须是已落图的 fact 节点 id（意图只能锚在已确认知识上）；predicted_vulnclass 必填（可证伪预测——写之前想清楚「看到什么算支持、什么算反驳」）。同一摘要的 open 意图已存在时不重复派生；frontier 积压超水位时会拒绝（等 planner 收口后再派）。派生成功后可传 diff_id 把对应 biz_layer_diff 标为 pushed。",
		objSchema(map[string]any{
			"task_id":             map[string]any{"type": "integer", "description": "目标任务 id（必填）"},
			"summary":             strParam("探索方向一句话：做什么+为什么（必填）"),
			"parent_fact_id":      map[string]any{"type": "integer", "description": "依据的 fact 节点 id（必填——业务建模的假设必须先落成 fact 再派生）"},
			"asset_ids":           map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "目标资产 id（尽量传：这条探索打哪些目标）"},
			"predicted_vulnclass": strParam("预测可能挖出的漏洞类别（必填，一个词，如 authz_bypass/info_disclosure）"),
			"predicted_actions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "预计动作清单（可证伪）"},
			"predicted_tools":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "预计工具名清单"},
			"kind":                strParam("种类：verify（验证/复测类，建模派生默认）| recon | exploit"),
			"priority":            map[string]any{"type": "integer", "description": "优先级 0-10，默认 5"},
			"diff_id":             map[string]any{"type": "integer", "description": "对应 biz_layer_diff id（可选；派生成功后标 pushed）"},
		}, "task_id", "summary", "parent_fact_id", "predicted_vulnclass"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID             int64    `json:"task_id"`
				Summary            string   `json:"summary"`
				ParentFactID       int64    `json:"parent_fact_id"`
				AssetIDs           []int64  `json:"asset_ids"`
				PredictedVulnclass string   `json:"predicted_vulnclass"`
				PredictedActions   []string `json:"predicted_actions"`
				PredictedTools     []string `json:"predicted_tools"`
				Kind               string   `json:"kind"`
				Priority           int      `json:"priority"`
				DiffID             int64    `json:"diff_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(strconv.FormatInt(a.TaskID, 10))
			if !ok {
				return actool.Errorf("task 不存在: " + strconv.FormatInt(a.TaskID, 10)), nil
			}
			store := t.Store
			if store == nil {
				return actool.Errorf(fmt.Sprintf("任务 %s 没有探索图（未启动或已归档）", t.ID)), nil
			}
			// ① 归一去重：同一业务假设不因措辞不同淹没 frontier。
			if id, dup, err := store.FindOpenIntentBySummary(a.Summary); err == nil && dup {
				return actool.Text(fmt.Sprintf("已有同摘要 open 意图 #%d，不重复派生（NormalizeIntentSummary 归一比对）。", id)), nil
			}
			// ② 积压水位感知：frontier 超阈时不派（与 planner 的 T5 水位同一判定源），
			//    一次派几十条会把 planner 切到「只收口」模式，派生反被吞。
			if raw, ok, _ := s.m.pg.GetSetting(agent.BacklogWatermarkSettingKey); ok {
				if wm, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && wm > 0 {
					if open, err := store.OpenIntentsCount(); err == nil && open >= wm {
						return actool.Text(fmt.Sprintf("frontier 积压 %d 条 ≥ 水位 %d，暂不派生——等 planner 收口后再试（积压水位与 planner 共用同一判定源）。", open, wm)), nil
					}
				}
			}
			// ③ 可证伪预测必填（F1 许愿纪律）：建模派生的意图必须自带
			//    「期待观察到什么」，否则运行后矛盾检测器无从对照。
			if strings.TrimSpace(a.PredictedVulnclass) == "" {
				return actool.Errorf("predicted_vulnclass 为必填：建模派生的意图必须可证伪——写之前想清楚「看到什么算支持这条假设、什么算反驳」"), nil
			}
			// ④ parent 校验：必须是已存在的 fact 节点（两段式的硬前置）。
			node, err := store.GetNode(a.ParentFactID)
			if err != nil || node == nil {
				return actool.Errorf(fmt.Sprintf("parent_fact_id %d 不存在：业务假设必须先落成 fact（record_fact 的委托路径），再由它派生意图", a.ParentFactID)), nil
			}
			if node.Kind != db.KindFact && node.Kind != db.KindFinding {
				return actool.Errorf(fmt.Sprintf("parent_fact_id %d 是 %s 节点：意图只能锚在 fact/finding 上（两段式：先 fact 后 intent）", a.ParentFactID, node.Kind)), nil
			}
			payload := map[string]any{
				"summary":              a.Summary,
				"predicted_vulnclass":  strings.TrimSpace(a.PredictedVulnclass),
				"kind":                 edgeKindOrVerify(a.Kind),
				"proposed_by":          "bizmodeler",
			}
			if len(a.PredictedActions) > 0 {
				payload["predicted_actions"] = a.PredictedActions
			}
			if len(a.PredictedTools) > 0 {
				payload["predicted_tools"] = a.PredictedTools
			}
			anchors := a.AssetIDs
			if len(anchors) == 0 {
				anchors = []int64{}
			}
			// 派生走 AddIntentInScopeWithCreator：锚点范围判定 + 资产拦截 + created_by
			// 全部继承基座写路径（不自己造闸门）。
			id, err := store.AddIntentInScopeWithCreator(a.TaskID, payload, a.Priority, anchors, "member", "bizmodeler")
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			msg := fmt.Sprintf("intent #%d derived（created_by=bizmodeler，kind=%s）", id, payload["kind"])
			if a.DiffID > 0 {
				if err := s.m.pg.UpdateBizDiffStatus(a.DiffID, "pushed", map[string]any{"intent_id": id}); err != nil {
					log.Printf("[biz] diff %d 标 pushed 失败: %v", a.DiffID, err)
				} else {
					msg += fmt.Sprintf("，biz diff #%d → pushed", a.DiffID)
				}
			}
			return actool.Text(msg), nil
		})
}

// toolBizRefutedFacts 读证伪池（增量建模的回滚依据，M7）。
func (s *Server) toolBizRefutedFacts() actool.CoreTool {
	return roTool("biz_refuted_facts",
		"读全库被证伪的事实摘要（fact 三态 refuted 态，近 N 条新在前）。增量建模前必读：业务图里引用了被证伪 fact 的关系要标 superseded（用 biz_adjudicate），对应 diff 要撤除——不撞证伪池是建模产物的准入门。",
		objSchema(map[string]any{"limit": map[string]any{"type": "integer", "description": "条数，默认 50"}}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			sums, err := s.m.pg.RefutedFactSummaries(a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{"refuted": sums, "count": len(sums)})
		})
}

// toolBizAdjudicate 冲突裁决的宿主侧执行入口（M7）。
//
// 「裁决写只在宿主侧发生」：模型（bizmodeler）只产出裁决建议并调本工具；真正的
// 写（supersede 指针 + 审计链）在本函数——db 层的唯一写函数 + AppendAuditEvent。
func (s *Server) toolBizAdjudicate() actool.CoreTool {
	return wrTool("biz_adjudicate",
		"冲突裁决的执行入口（宿主侧落笔+审计）。三种动作：supersede_asset（资产图记录与实际行为矛盾、己方证据更强时：old_asset_id 指向 new_asset_id，旧记录标失效；证据同级且差距小时不要调用——转人工）；supersede_biz_node（业务关系被证伪：old_node_id 指向 new_node_id）；revert_diff（人工/复核判定某条 diff 是误报：diff_id 标 false_positive）。每次调用都进审计哈希链，可被 /api/audit/chain 复算。",
		objSchema(map[string]any{
			"action":      strParam("supersede_asset|supersede_biz_node|revert_diff——必填"),
			"old_asset_id": map[string]any{"type": "integer", "description": "supersede_asset：旧资产 id"},
			"new_asset_id": map[string]any{"type": "integer", "description": "supersede_asset：新资产 id"},
			"old_node_id":  map[string]any{"type": "integer", "description": "supersede_biz_node：旧业务节点 id"},
			"new_node_id":  map[string]any{"type": "integer", "description": "supersede_biz_node：新业务节点 id"},
			"diff_id":      map[string]any{"type": "integer", "description": "revert_diff：差异 id"},
			"reason":       strParam("裁决理由（必填；写进审计链）"),
		}, "action", "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Action      string `json:"action"`
				OldAssetID  int64  `json:"old_asset_id"`
				NewAssetID  int64  `json:"new_asset_id"`
				OldNodeID   int64  `json:"old_node_id"`
				NewNodeID   int64  `json:"new_node_id"`
				DiffID      int64  `json:"diff_id"`
				Reason      string `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			reason := strings.TrimSpace(a.Reason)
			if reason == "" {
				return actool.Errorf("reason 为必填：裁决必须可追溯"), nil
			}
			var detail string
			switch a.Action {
			case "supersede_asset":
				if a.OldAssetID <= 0 || a.NewAssetID <= 0 || a.OldAssetID == a.NewAssetID {
					return actool.Errorf("old_asset_id/new_asset_id 必填且不能相同"), nil
				}
				if err := s.m.pg.SupersedeAsset(a.OldAssetID, a.NewAssetID); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				detail = fmt.Sprintf("asset #%d superseded by #%d", a.OldAssetID, a.NewAssetID)
				s.appendBizAudit("biz-supersede", detail, reason, map[string]any{
					"action": a.Action, "old": a.OldAssetID, "new": a.NewAssetID,
				})
			case "supersede_biz_node":
				if a.OldNodeID <= 0 || a.NewNodeID <= 0 || a.OldNodeID == a.NewNodeID {
					return actool.Errorf("old_node_id/new_node_id 必填且不能相同"), nil
				}
				if err := s.m.pg.MarkBizNodeSuperseded(a.OldNodeID, a.NewNodeID); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				detail = fmt.Sprintf("biz node #%d superseded by #%d", a.OldNodeID, a.NewNodeID)
				s.appendBizAudit("biz-supersede", detail, reason, map[string]any{
					"action": a.Action, "old": a.OldNodeID, "new": a.NewNodeID,
				})
			case "revert_diff":
				if a.DiffID <= 0 {
					return actool.Errorf("diff_id 必填"), nil
				}
				if err := s.m.pg.UpdateBizDiffStatus(a.DiffID, "false_positive", nil); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				detail = fmt.Sprintf("biz diff #%d → false_positive", a.DiffID)
				s.appendBizAudit("biz-adjudication", detail, reason, map[string]any{
					"action": a.Action, "diff_id": a.DiffID,
				})
			default:
				return actool.Errorf("action 必须是 supersede_asset|supersede_biz_node|revert_diff"), nil
			}
			return actool.Text("裁决落笔完成（已进审计链）：" + detail), nil
		})
}

// edgeKindOrVerify 把模型给的自由文本种类归一；空/认不出时建模派生默认 verify
// （业务假设的验证是建模派生的主导语义，general 兜底会绕过种类容量闸）。
func edgeKindOrVerify(raw string) string {
	if k := db.NormalizeIntentKind(raw); k != db.IntentKindGeneral {
		return k
	}
	return db.IntentKindVerify
}

// appendBizAudit 是业务建模侧动作的审计链双写（M1 govRecord 同款：观测可以丢，
// 链不可丢——AppendAuditEvent 失败只记日志不回滚业务写）。
func (s *Server) appendBizAudit(source, action, reason string, body map[string]any) {
	b, _ := json.Marshal(body)
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "bizmodeler",
		ToolName: "biz",
		Action:   action,
		Reason:   reason,
		Payload:  b,
		Source:   source,
	}); err != nil {
		log.Printf("[biz] 审计链写入失败（忽略）: %v", err)
	}
}
