package server

// 方法论配置的 REST 与合并视图（0.14.7，批次六 F2）：
//
//	GET  /api/edge/methodologies          合并视图（内置 ∪ 自定义，覆盖行生效）
//	PUT  /api/edge/methodologies/{id}     写配置（内置=启停/排序覆盖；自定义=全量 upsert）
//	DELETE /api/edge/methodologies/{id}   删自定义（内置行=恢复默认：删覆盖）
//	GET  /api/edge/methodologies/export   导出 JSON（内置全量+自定义）
//	POST /api/edge/methodologies/import   导入 JSON（逐条过 internal/edge 同套校验）
//
// 红线：内置方法论【本体】与判据在代码常量（internal/edge 注册表，编译期钉死）；
// 导入的内/外部数据只能新增自定义条目或覆盖内置的启停/排序——覆盖不了判据。

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/edge"
)

// methodologyPayload 是导入/导出的统一 JSON 形状（internal/edge.Methodology 的
// JSON 友好投影：criterion 走它自己的 json tag）。
type methodologyPayload struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Tier          int                     `json:"tier"`
	EvidenceGrade string                  `json:"evidence_grade"`
	Summary       string                  `json:"summary"`
	Criterion     *edge.CompletionCriterion `json:"criterion,omitempty"`
	HeuristicTags []string                `json:"heuristic_tags"` // 无 omitempty：空标签也输出 []，旧前端对 undefined.length 无防御（0.14.7 序列化回归根因）
	Enabled       bool                    `json:"enabled"`
	Priority      int                     `json:"priority"`
	Source        string                  `json:"source,omitempty"` // 导出时标注
}

// mergedMethodologies 是配置表 + 内置注册表的合并视图。
func (s *Server) mergedMethodologies() ([]methodologyPayload, error) {
	pg := s.m.pg
	rows, err := pg.ListEdgeMethodologies()
	if err != nil {
		return nil, err
	}
	overrides := map[string]db.EdgeMethodologyRow{}
	customs := []methodologyPayload{}
	for _, r := range rows {
		if r.Source == "builtin-override" {
			overrides[r.ID] = r
			continue
		}
		// custom：构造 Methodology 过同一套注册期校验（校验规则=代码常量）。
		m := edge.Methodology{
			ID: r.ID, Name: r.Name, Tier: r.Tier,
			EvidenceGrade: edge.EvidenceGrade(r.EvidenceGrade),
			Summary:       r.Summary, HeuristicTags: r.HeuristicTags,
			Enabled: r.Enabled, Priority: r.Priority,
		}
		if len(r.Criterion) > 0 && string(r.Criterion) != "null" {
			var c edge.CompletionCriterion
			if err := json.Unmarshal(r.Criterion, &c); err != nil {
				continue // 损坏行跳过（导入时校验过，这里是防御）
			}
			m.Criterion = &c
		}
		if err := m.Criterion.Validate(); (m.Tier == 1 || m.Tier == 2) && err != nil {
			continue // 校验不过的 custom 行跳过（不进视图）
		}
		customs = append(customs, methodologyPayload{
			ID: m.ID, Name: m.Name, Tier: m.Tier,
			EvidenceGrade: string(m.EvidenceGrade), Summary: m.Summary,
			Criterion: m.Criterion, HeuristicTags: m.HeuristicTags,
			Enabled: m.Enabled, Priority: m.Priority, Source: "custom",
		})
	}
	out := []methodologyPayload{}
	for _, m := range edge.ListAll() {
		tags := m.HeuristicTags
		if tags == nil {
			tags = []string{} // nil → []（前端 undefined/null 防御双保险，0.15.1 根因修复）
		}
		p := methodologyPayload{
			ID: m.ID, Name: m.Name, Tier: m.Tier,
			EvidenceGrade: string(m.EvidenceGrade), Summary: m.Summary,
			Criterion: m.Criterion, HeuristicTags: tags,
			Enabled: m.Enabled, Priority: m.Priority, Source: "builtin",
		}
		if ov, ok := overrides[m.ID]; ok {
			p.Enabled = ov.Enabled
			p.Priority = ov.Priority
			p.Source = "builtin-override" // 前端据此区分「恢复默认」与「删除」（导入侧不读 source，无回环影响）
		}
		out = append(out, p)
	}
	out = append(out, customs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out, nil
}

func (s *Server) edgeMethodologiesMerged(w http.ResponseWriter, _ *http.Request) {
	out, err := s.mergedMethodologies()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"methodologies": out})
}

func (s *Server) edgeMethodologyPut(w http.ResponseWriter, r *http.Request) {
	var p methodologyPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	p.ID = r.PathValue("id")
	_, isBuiltin := edge.Get(p.ID)
	source := "custom"
	if isBuiltin {
		// 内置条目：只接受启停/排序覆盖（本体与判据不可被外部数据改）；
		// 覆盖行的本体字段从代码常量补齐（payload 可能只带 enabled/priority）。
		if p.Name != "" && p.Name != edgeMustName(p.ID) {
			writeErr(w, 400, "内置方法论的本体不可修改：只能改 enabled/priority")
			return
		}
		p.Name, p.Tier, p.EvidenceGrade, p.Summary, p.Criterion, p.HeuristicTags = edgeMustAll(p.ID)
		source = "builtin-override"
	}
	// 自定义条目：全量过注册期校验（同 internal/edge.Register 的硬约束）。
	m := edge.Methodology{
		ID: p.ID, Name: p.Name, Tier: p.Tier,
		EvidenceGrade: edge.EvidenceGrade(p.EvidenceGrade),
		Summary:       p.Summary, Criterion: p.Criterion,
		HeuristicTags: p.HeuristicTags, Enabled: p.Enabled, Priority: p.Priority,
	}
	if !isBuiltin {
		if err := edge.Register(m); err != nil {
			// 注册失败=校验拒（重复/缺判据/隐喻带判据等）——Register 会污染进程
			// map 吗？不会：Register 校验失败时不写 map（先校验后赋值）。
			writeErr(w, 400, err.Error())
			return
		}
		// 校验过的条目从进程 map 移除（它是持久化条目，读取时再校验合成——
		// 不参与本轮内置轮换，避免重启前后的行为漂移）。
		edge.Unregister(p.ID)
	} else {
		// 内置覆盖行：tier/evidence 不改动（保留 payload 校验语义）。
		if _, ok := edge.Get(p.ID); !ok {
			writeErr(w, 404, "内置方法论不存在")
			return
		}
	}
	row := db.EdgeMethodologyRow{
		ID: p.ID, Source: source, Name: p.Name, Tier: p.Tier,
		EvidenceGrade: p.EvidenceGrade, Summary: p.Summary,
		HeuristicTags: p.HeuristicTags, Enabled: p.Enabled, Priority: p.Priority,
	}
	if p.Criterion != nil {
		if b, err := json.Marshal(p.Criterion); err == nil {
			row.Criterion = b
		}
	}
	if err := s.m.pg.UpsertEdgeMethodology(row); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "source": source})
}

func (s *Server) edgeMethodologyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, isBuiltin := edge.Get(id); isBuiltin {
		// 内置覆盖行删除=恢复默认（启停/排序回到代码常量值）。
		if err := s.m.pg.DeleteEdgeMethodologyBuiltinOverride(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "restored_default": true})
		return
	}
	if err := s.m.pg.DeleteEdgeMethodology(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) edgeMethodologyExport(w http.ResponseWriter, _ *http.Request) {
	out, err := s.mergedMethodologies()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"methodologies": out, "version": 1})
}

func (s *Server) edgeMethodologyImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Methodologies []methodologyPayload `json:"methodologies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体非法")
		return
	}
	applied, rejected := []string{}, []string{}
	for _, p := range req.Methodologies {
		_, isBuiltin := edge.Get(p.ID)
		source := "custom"
		if isBuiltin {
			source = "builtin-override"
			p.Name, p.Tier, p.EvidenceGrade, p.Summary, p.Criterion, p.HeuristicTags = edgeMustAll(p.ID)
		}
		// 逐条过注册期校验（自定义全量校验；内置覆盖行只落启停/排序）。
		row := db.EdgeMethodologyRow{
			ID: p.ID, Source: source, Name: p.Name, Tier: p.Tier,
			EvidenceGrade: p.EvidenceGrade, Summary: p.Summary,
			HeuristicTags: p.HeuristicTags, Enabled: p.Enabled, Priority: p.Priority,
		}
		if p.Criterion != nil {
			if b, e := json.Marshal(p.Criterion); e == nil {
				row.Criterion = b
			}
		}
		if !isBuiltin {
			m := edge.Methodology{
				ID: p.ID, Name: p.Name, Tier: p.Tier,
				EvidenceGrade: edge.EvidenceGrade(p.EvidenceGrade),
				Summary:       p.Summary, Criterion: p.Criterion,
				HeuristicTags: p.HeuristicTags, Enabled: p.Enabled, Priority: p.Priority,
			}
			if err := edge.Register(m); err != nil {
				rejected = append(rejected, p.ID)
				continue
			}
			edge.Unregister(p.ID)
		}
		if err := s.m.pg.UpsertEdgeMethodology(row); err != nil {
			rejected = append(rejected, p.ID)
			continue
		}
		applied = append(applied, p.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "applied": applied, "rejected": rejected})
}

// edgeMustName / edgeMustAll 取内置条目字段（isBuiltin 已确认时调用）。
func edgeMustName(id string) string {
	if m, ok := edge.Get(id); ok {
		return m.Name
	}
	return ""
}

func edgeMustAll(id string) (name string, tier int, grade, summary string, crit *edge.CompletionCriterion, tags []string) {
	m, ok := edge.Get(id)
	if !ok {
		return
	}
	return m.Name, m.Tier, string(m.EvidenceGrade), m.Summary, m.Criterion, m.HeuristicTags
}
