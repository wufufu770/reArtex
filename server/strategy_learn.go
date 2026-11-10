package server

// 策略库自学习与版本化编辑（方案 v10 §9 E2-2 / E2-3）。
//
// E2-2 摄入：URL 或粘贴文本 → 平台侧抓取 → 一次 LLM 总结出候选策略（JSON 数组）
// → strategy proposal（pending 隔离态）→ proposals 页人工裁决 → curator 三门晋级。
// 完全复用既有隔离态/晋级链，不新建状态机。
//
// E2-3 编辑：宿主裁决面的版本化写——version+1、不原地覆盖语义由版本号承载，
// 编辑动作写 audit_events（哈希链）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func mustJSONValue(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// dbUpdateStrategyEditable 是 PATCH 的落库（版本化：version=version+1）。
func (s *Server) dbUpdateStrategyEditable(id int64, sets map[string]any) (int64, error) {
	if len(sets) == 0 {
		return 0, fmt.Errorf("没有可更新字段")
	}
	cols := []string{"name", "summary", "activation", "steps", "preconditions",
		"tool_requirements", "success_criteria", "failure_modes", "parameters", "applies_to"}
	args := []any{}
	clauses := []string{}
	for _, c := range cols {
		if v, ok := sets[c]; ok {
			args = append(args, v)
			clauses = append(clauses, fmt.Sprintf("%s=$%d", c, len(args)))
		}
	}
	if len(clauses) == 0 {
		return 0, fmt.Errorf("没有可更新字段")
	}
	args = append(args, id)
	var version int64
	err := s.m.pg.QueryRow(fmt.Sprintf(`
UPDATE strategies SET %s, version=version+1, updated_at=now()
WHERE id=$%d RETURNING version`, strings.Join(clauses, ", "), len(args)), args...).Scan(&version)
	return version, err
}

// strategyEdit PATCH /api/strategies/{id} —— 宿主裁决面的版本化编辑。
func (s *Server) strategyEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad strategy id")
		return
	}
	var req struct {
		Name             *string          `json:"name"`
		Summary          *string          `json:"summary"`
		Activation       *json.RawMessage `json:"activation"`
		Steps            *json.RawMessage `json:"steps"`
		Preconditions    *json.RawMessage `json:"preconditions"`
		ToolRequirements *json.RawMessage `json:"tool_requirements"`
		SuccessCriteria  *json.RawMessage `json:"success_criteria"`
		FailureModes     *json.RawMessage `json:"failure_modes"`
		Parameters       *json.RawMessage `json:"parameters"`
		AppliesTo        *json.RawMessage `json:"applies_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sets := map[string]any{}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			writeErr(w, 400, "name 不能为空")
			return
		}
		sets["name"] = n
	}
	if req.Summary != nil {
		sets["summary"] = *req.Summary
	}
	for _, pair := range []struct {
		key string
		raw *json.RawMessage
	}{{"activation", req.Activation}, {"steps", req.Steps}, {"preconditions", req.Preconditions},
		{"tool_requirements", req.ToolRequirements}, {"success_criteria", req.SuccessCriteria},
		{"failure_modes", req.FailureModes}, {"parameters", req.Parameters}, {"applies_to", req.AppliesTo}} {
		if pair.raw == nil {
			continue
		}
		if !json.Valid([]byte(*pair.raw)) {
			writeErr(w, 400, pair.key+" 不是合法 JSON")
			return
		}
		sets[pair.key] = []byte(*pair.raw)
	}
	version, err := s.dbUpdateStrategyEditable(id, sets)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_, _ = s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "strategies", Action: "strategy.edit",
		Reason: fmt.Sprintf("人工编辑策略 #%d（版本化写）", id),
		Payload: mustJSONValue(map[string]any{"id": id, "fields": func() []string {
			ks := []string{}
			for k := range sets {
				ks = append(ks, k)
			}
			return ks
		}(), "version": version}),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "version": version})
}

// strategyIngest POST /api/strategies/ingest —— 报告/网址 → 总结学习 → 候选策略提议。
func (s *Server) strategyIngest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	source := strings.TrimSpace(req.URL)
	var material string
	if source != "" {
		fetched, err := fetchForIngest(source)
		if err != nil {
			writeErr(w, 400, "抓取失败: "+err.Error())
			return
		}
		material = fetched
	} else if strings.TrimSpace(req.Text) != "" {
		material = req.Text
	} else {
		writeErr(w, 400, "需要 url 或 text 之一")
		return
	}
	if len([]rune(material)) < 120 {
		writeErr(w, 400, "素材太短（<120 字），没有可学习的内容")
		return
	}
	if len(material) > 200_000 {
		material = material[:200_000]
	}

	prov, ok := s.sodProviderFunc()(0)
	if !ok || prov == nil {
		writeErr(w, 503, "没有可用的 LLM 配置——先在系统页激活一个配置")
		return
	}
	temp := 0.2
	reqMsg := llm.CompletionRequest{
		System: []string{ingestSystemPrompt()},
		Messages: []llm.Message{llm.UserText("【待学习素材】\n" + material)},
		MaxTokens:   8192,
		Temperature: &temp,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	acc := llm.NewAccumulator()
	for ev, err := range prov.Stream(ctx, reqMsg) {
		if err != nil {
			writeErr(w, 502, "学习 LLM 调用失败: "+err.Error())
			return
		}
		acc.Add(ev)
	}
	items := parseIngestItems(acc.Message().Text())
	if len(items) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "proposals": 0, "note": "模型判定素材中没有可沉淀的策略内容"})
		return
	}
	created := []int64{}
	for _, it := range items {
		if strings.TrimSpace(it.Name) == "" || strings.TrimSpace(it.Summary) == "" {
			continue
		}
		payload := mustJSONValue(it)
		pid, err := s.m.pg.CreateStrategyProposal(&db.StrategyProposal{
			Route:     "create_new",
			Payload:   payload,
			Status:    "pending",
			Reason:    "策略摄入学习（E2-2）：" + trunc(source, 120),
			CreatedBy: "ingest",
		})
		if err == nil {
			created = append(created, pid)
		}
	}
	_, _ = s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor: "operator", ToolName: "strategies", Action: "strategy.ingest",
		Reason: "摄入学习产出候选策略", Payload: mustJSONValue(map[string]any{"source": source, "count": len(created)}),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "proposals": len(created), "ids": created})
}

// fetchForIngest 平台侧抓取（30s 超时 + 2MB 截断；不冒充任务出网，审计在调用方留痕）。
func fetchForIngest(rawURL string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_000))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ingestSystemPrompt 是摄入学习的生成契约。
func ingestSystemPrompt() string {
	return `你是策略策展人。从给定素材（报告/文章/网页）中提炼【可复用的渗透策略卡】。
输出严格的 JSON 数组（不要 markdown 代码块、不要解释），每条：
{"name":"策略名（≤24字）","summary":"一句话打法","activation":["适用条件1","适用条件2"],
 "steps":["步骤1","步骤2"],"success_criteria":["客观验证1"]}
约束：最多 5 条；只提炼素材里真实讲到的方法（禁止脑补）；素材是安全报告/技术文章时提取其攻击打法与验证方式；
素材与渗透无关时输出 []。全部中文。`
}

// parseIngestItems 解析模型产出（容忍 markdown 代码块包裹）。
func parseIngestItems(text string) []struct {
	Name            string   `json:"name"`
	Summary         string   `json:"summary"`
	Activation      []string `json:"activation"`
	Steps           []string `json:"steps"`
	SuccessCriteria []string `json:"success_criteria"`
} {
	type item = struct {
		Name            string   `json:"name"`
		Summary         string   `json:"summary"`
		Activation      []string `json:"activation"`
		Steps           []string `json:"steps"`
		SuccessCriteria []string `json:"success_criteria"`
	}
	out := []item{}
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	start := strings.Index(t, "[")
	end := strings.LastIndex(t, "]")
	if start < 0 || end <= start {
		return out
	}
	if err := json.Unmarshal([]byte(t[start:end+1]), &out); err != nil {
		return nil
	}
	return out
}
