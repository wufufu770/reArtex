package server

// 方法论配置的 REST 与合并视图（0.14.7，批次六 F2；0.15.4 全量可编+写穿运行时）：
//
//	GET  /api/edge/methodologies          合并视图（内置 ∪ 自定义，覆盖行生效）
//	PUT  /api/edge/methodologies/{id}     写配置（内置=全量覆盖存 builtin-override，可恢复默认；自定义=全量 upsert）
//	DELETE /api/edge/methodologies/{id}   删自定义（内置行=恢复默认：删覆盖+运行时回退代码常量）
//	GET  /api/edge/methodologies/export   导出 JSON（内置全量+自定义）
//	POST /api/edge/methodologies/import   导入 JSON（逐条过 internal/edge 同套校验）
//
// 0.15.4 起内置条目【本体】也可经交互面全量编辑（用户授权：同套校验 +
// builtin-override 持久化 + 恢复默认随时回退代码常量），且配置写穿运行时
// 注册表——轮换/判定/agent 工具读到的即合并后状态（此前只改展示不生效，
// 自定义条目甚至进不了轮换）。批量导入面维持只落启停/排序：外部数据不注入本体。

import (
	"encoding/json"
	"log"
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

// rowCriterion 解出配置行的判据 JSON（无/损坏返回 nil）。
func rowCriterion(raw []byte) *edge.CompletionCriterion {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var c edge.CompletionCriterion
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	return &c
}

// mergedMethodologies 是配置表 + 内置注册表的合并视图。覆盖行全量生效：
// 0.15.4 起内置覆盖行携带本体（PUT 全量写入；0.15.4 前的旧行本体字段=当时
// 从代码常量补齐的值，全量读行与读常量等价）。
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
			Criterion: rowCriterion(r.Criterion),
		}
		if err := validatePayloadMethodology(m); (m.Tier == 1 || m.Tier == 2) && err != nil {
			continue // 校验不过的 custom 行跳过（不进视图）
		}
		tags := m.HeuristicTags
		if tags == nil {
			tags = []string{}
		}
		customs = append(customs, methodologyPayload{
			ID: m.ID, Name: m.Name, Tier: m.Tier,
			EvidenceGrade: string(m.EvidenceGrade), Summary: m.Summary,
			Criterion: m.Criterion, HeuristicTags: tags,
			Enabled: m.Enabled, Priority: m.Priority, Source: "custom",
		})
	}
	out := []methodologyPayload{}
	for _, m := range edge.ListAll() {
		p := methodologyPayload{
			ID: m.ID, Name: m.Name, Tier: m.Tier,
			EvidenceGrade: string(m.EvidenceGrade), Summary: m.Summary,
			Criterion: m.Criterion, HeuristicTags: tagsOrEmpty(m.HeuristicTags),
			Enabled: m.Enabled, Priority: m.Priority, Source: "builtin",
		}
		if ov, ok := overrides[m.ID]; ok {
			// 覆盖行全量生效（运行时注册表同样已被写穿——两处一致）。
			p = methodologyPayload{
				ID: ov.ID, Name: ov.Name, Tier: ov.Tier,
				EvidenceGrade: ov.EvidenceGrade, Summary: ov.Summary,
				Criterion: rowCriterion(ov.Criterion),
				HeuristicTags: tagsOrEmpty(ov.HeuristicTags),
				Enabled:       ov.Enabled, Priority: ov.Priority,
				Source: "builtin-override",
			}
		}
		out = append(out, p)
	}
	out = append(out, customs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out, nil
}

func tagsOrEmpty(t []string) []string {
	if t == nil {
		return []string{} // nil → []（前端 undefined/null 防御，0.15.1 根因口径）
	}
	return t
}

// validatePayloadMethodology 复用注册表硬约束（不落表，只判合法性）。
func validatePayloadMethodology(m edge.Methodology) error {
	return edge.UpsertValidate(m)
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
	source := "custom"
	if edge.IsBuiltin(p.ID) {
		// 内置条目：全量覆盖（本体/判据/标签/说明 + 启停/优先级），存
		// builtin-override——同套校验，恢复默认随时回退代码常量（0.15.4）。
		source = "builtin-override"
	}
	// 全量过注册期同套校验（UpsertValidate 的规则）并写穿运行时注册表：
	// 轮换（ListEnabled）、判定（Get）、agent 工具（ListAll）立即生效。
	m := edge.Methodology{
		ID: p.ID, Name: p.Name, Tier: p.Tier,
		EvidenceGrade: edge.EvidenceGrade(p.EvidenceGrade),
		Summary:       p.Summary, Criterion: p.Criterion,
		HeuristicTags: p.HeuristicTags, Enabled: p.Enabled, Priority: p.Priority,
	}
	if err := edge.Upsert(m); err != nil {
		writeErr(w, 400, err.Error())
		return
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
	if edge.IsBuiltin(id) {
		// 内置覆盖行删除=恢复默认（删 DB 覆盖 + 运行时回退代码常量本体）。
		if err := s.m.pg.DeleteEdgeMethodologyBuiltinOverride(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if _, ok := edge.RestoreBuiltin(id); !ok {
			log.Printf("[edge] 恢复默认 %s：运行时无内置备份（仅告警，DB 已回退）", id)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "restored_default": true})
		return
	}
	if err := s.m.pg.DeleteEdgeMethodology(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	edge.Unregister(id) // 写穿反向：自定义删除后从运行时注册表移除（0.15.4）
	writeJSON(w, 200, map[string]any{"ok": true})
}

// syncEdgeRegistry 把 DB 方法论配置写穿进程注册表（0.15.4）：builtin-override
// 行全量覆盖（本体+启停+优先级），custom 行注册。启动一次 + 配写后调用；
// 校验不过的行跳过并记日志（不阻断启动，运行时退回代码常量）。
func (s *Server) syncEdgeRegistry() {
	rows, err := s.m.pg.ListEdgeMethodologies()
	if err != nil {
		log.Printf("[edge] 方法论配置同步失败（运行时用代码常量）: %v", err)
		return
	}
	for _, r := range rows {
		m := edge.Methodology{
			ID: r.ID, Name: r.Name, Tier: r.Tier,
			EvidenceGrade: edge.EvidenceGrade(r.EvidenceGrade),
			Summary:       r.Summary, HeuristicTags: r.HeuristicTags,
			Enabled: r.Enabled, Priority: r.Priority,
			Criterion: rowCriterion(r.Criterion),
		}
		if err := edge.Upsert(m); err != nil {
			log.Printf("[edge] 配置行 %s(%s) 校验不过，跳过: %v", r.ID, r.Source, err)
		}
	}
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
			if err := edge.UpsertValidate(m); err != nil {
				rejected = append(rejected, p.ID)
				continue
			}
		}
		if err := s.m.pg.UpsertEdgeMethodology(row); err != nil {
			rejected = append(rejected, p.ID)
			continue
		}
		applied = append(applied, p.ID)
	}
	s.syncEdgeRegistry() // 导入写穿运行时（0.15.4：自定义条目立即参与轮换/判定）
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
