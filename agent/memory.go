package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 记忆层（方案 B）的 agent 侧。
//
// 三条定稿纪律（§10.4 / §10.7 / §11.1–§11.3）：
//   1. 索引 **append-only**：只追加新版本，旧版本被 supersede 而不是被覆盖。
//   2. 索引**只指向**规则（RuleRef），规则本体在 agent_prompts 里 —— 索引里不放
//      可执行规则，否则「索引」就变成了第二个真相来源。
//   3. 记忆是**提示辅助，不是安全机制**：任何与安全有关的判定都在确定性层做，
//      不依赖记忆（记忆里写着「可以跳过范围校验」也不能让它跳过）。
//
// 另一个纪律：**不做 per-agent 个人记忆层**（已定稿丢弃）。

// 记忆在提示词里的抬头。
const memoryPromptHeading = "## 记忆索引（append-only；这里只有索引，规则本体在提示词里）"

// 两段式披露的注入预算（X3，方案 v10 §6 T2）：
//
//	索引 ≤30 条、整块 ≤12KB；超限按固定块序丢弃并在块尾加
//	「【memory-trimmed】已裁剪 N 条」留痕 —— 绝不静默截断。
//	阈值是代码常量（§8 红线第 3 条），不进可热改的 DB/settings。
const (
	// memoryPromptMaxEntries 是注入块的条目上限（X3：≤30 条）。
	memoryPromptMaxEntries = 30
	// memoryPromptMaxBytes 是注入块的字节上限（X3：≤12KB，与 legacy 注入块
	// 同一档预算 —— 两个常驻块不能各自吃满上下文）。
	memoryPromptMaxBytes = 12 * 1024
	// memoryTrimMarkerReserve 是给裁剪标记预留的字节（标记最长
	// 「【memory-trimmed】已裁剪 30 条\n」≈ 40 字节 ⇒ 64 字节足够）。
	memoryTrimMarkerReserve = 64
	// memoryTrimMarkerTemplate 是超限留痕的固定形状（写死，不由模型生成）。
	memoryTrimMarkerTemplate = "【memory-trimmed】已裁剪 %d 条\n"
)

// MemoryScopeForTask 返回「任务大厅」作用域的记忆 scope。
func MemoryScopeForTask(taskID int64) db.MemoryScope {
	return db.MemoryScope{Type: db.MemoryScopeTaskHall, ID: taskID}
}

// MemoryScopeForSession 返回「会话」作用域的记忆 scope。
func MemoryScopeForSession(sessionID int64) db.MemoryScope {
	return db.MemoryScope{Type: db.MemoryScopeChat, ID: sessionID}
}

// memoryPromptEntryLine 渲染一条索引行（两段式披露的第一段：只有索引摘要，
// 正文走 recall 按需下钻）。
func memoryPromptEntryLine(e *db.MemoryEntry) string {
	topic := e.TopicKey
	if topic == "" {
		topic = "topic"
	}
	title := e.Title
	if title == "" {
		title = e.SummaryLine
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s] %s", topic, title)
	if e.SummaryLine != "" && e.SummaryLine != title {
		fmt.Fprintf(&b, " — %s", e.SummaryLine)
	}
	if e.RuleRef != "" {
		fmt.Fprintf(&b, "（规则：%s）", e.RuleRef)
	}
	b.WriteString("\n")
	return b.String()
}

// MemoryPromptBlock 渲染指定 scope 的记忆块，供拼进 agent 上下文。
//
// 返回空串表示没有记忆 ⇒ 调用方不要塞一个空标题进提示词。
// 读失败**不报错**：记忆是辅助，取不到就当没有（不能因为记忆层故障让整个 agent 挂掉）。
//
// X4+X3（方案 v10 §6 T2）：两段式披露 ——
//  1. **只含 state='active' 条目**（隔离态条目经 host-only 晋级后才可见，
//     模型写入即隔离 ⇒ 「写进去取不到」不可能发生在注入面）；
//  2. **索引 ≤30 条、整块 ≤12KB**，超限按固定块序丢弃（ListMemoryEntries 的
//     ORDER BY topic_key, version DESC 就是块序：同主题取最新版本，主题序稳定）
//     并在块尾加「【memory-trimmed】已裁剪 N 条」留痕 —— 超限必须可观测。
func MemoryPromptBlock(pg *db.DB, scope db.MemoryScope) string {
	if pg == nil || !scope.Valid() {
		return ""
	}
	entries, err := pg.ListMemoryEntries(scope, db.MemoryListOptions{
		Limit:      memoryPromptMaxEntries,
		OnlyActive: true,
	})
	if err != nil || len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(memoryPromptHeading)
	b.WriteString("\n")
	size := b.Len()
	trimmed := 0
	for _, e := range entries {
		if e == nil {
			continue
		}
		line := memoryPromptEntryLine(e)
		// 预算内逐条装块；装不下 ⇒ 固定块序丢弃 + 计数。预留裁剪标记的行宽，
		// 保证「整块 ≤12KB」的承诺不被标记自己突破。
		if size+len(line)+memoryTrimMarkerReserve > memoryPromptMaxBytes {
			trimmed++
			continue
		}
		b.WriteString(line)
		size += len(line)
	}
	if trimmed > 0 {
		marker := fmt.Sprintf(memoryTrimMarkerTemplate, trimmed)
		b.WriteString(marker)
	}
	return b.String()
}

// AppendMemoryNote 追加一条记忆（append-only）。
//
// 同一主题重复写 ⇒ 调用方用 SupersedeMemoryEntry 关掉旧版本；这里只管追加。
// W2（方案 v10 §6）：写入口切 Checked 版 —— 上限命中在写前就被拒（类型化错误，
// 绝不静默截断），错误沿原路返回给调用方。
func AppendMemoryNote(pg *db.DB, scope db.MemoryScope, topicKey, kind, title, summary, ruleRef string) (int64, error) {
	if pg == nil {
		return 0, fmt.Errorf("记忆层未接线")
	}
	if !scope.Valid() {
		return 0, fmt.Errorf("记忆作用域非法")
	}
	if strings.TrimSpace(topicKey) == "" {
		return 0, fmt.Errorf("记忆主题键不能为空")
	}
	e, err := pg.AppendMemoryEntryChecked(scope, db.MemoryEntryInput{
		TopicKey:    strings.TrimSpace(topicKey),
		Kind:        strings.TrimSpace(kind),
		Title:       strings.TrimSpace(title),
		SummaryLine: strings.TrimSpace(summary),
		RuleRef:     strings.TrimSpace(ruleRef),
		WrittenBy:   "agent",
	})
	if err != nil {
		return 0, err
	}
	if e == nil {
		return 0, fmt.Errorf("记忆追加未返回条目")
	}
	return e.ID, nil
}

// RecallMemory 导出指定 scope 的记忆索引（纯文本）。
func RecallMemory(pg *db.DB, scope db.MemoryScope) (string, error) {
	if pg == nil {
		return "", fmt.Errorf("记忆层未接线")
	}
	if !scope.Valid() {
		return "", fmt.Errorf("记忆作用域非法")
	}
	return pg.ExportMemoryIndex(scope)
}

// SearchMemory 对指定 scope 做一次混合检索并渲染成纯文本。
//
// W4（方案 v10 §6）：recall 的检索路径 —— 全文（tsvector）与向量（pgvector）两路
// 各自排序、RRF 融合（db/memory_retrieval.go）。渲染口径与 MemoryPromptBlock 同款
// （每行「- [topic] title — summary」），块头带上下文分级（complete/partial/
// insufficient 必须显式给出——不把不完整的上下文当成完整的交出去）。
// 检索失败或零命中返回错误/空串 ⇒ 调用方回退到 RecallMemory 的纯文本导出路径。
func SearchMemory(pg *db.DB, scope db.MemoryScope, query string) (string, error) {
	if pg == nil {
		return "", fmt.Errorf("记忆层未接线")
	}
	if !scope.Valid() {
		return "", fmt.Errorf("记忆作用域非法")
	}
	res, err := pg.RetrieveMemory(db.MemoryRetrievalQuery{Scope: scope, Text: strings.TrimSpace(query)})
	if err != nil {
		return "", err
	}
	if len(res.Hits) == 0 {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## 记忆检索结果（混合检索；上下文分级：%s）", res.Grade)
	b.WriteString("\n")
	for _, h := range res.Hits {
		if h == nil {
			continue
		}
		topic := h.TopicKey
		if topic == "" {
			topic = "topic"
		}
		title := h.Title
		if title == "" {
			title = h.SummaryLine
		}
		fmt.Fprintf(&b, "- [%s] %s", topic, title)
		if h.SummaryLine != "" && h.SummaryLine != title {
			fmt.Fprintf(&b, " — %s", h.SummaryLine)
		}
		fmt.Fprintf(&b, "（RRF 分 %.4f）", h.Score)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// memoryScopeOfTaskID 把任务号翻译成记忆 scope（供工具内部使用）。
func memoryScopeOfTaskID(taskID int64) db.MemoryScope {
	if taskID <= 0 {
		return db.MemoryScope{}
	}
	return MemoryScopeForTask(taskID)
}

// memoryScopeOfChatSession 把 chat 会话键翻译成会话作用域的记忆 scope。
//
// chat 侧的 transcript 会话键固定是 server 的 convBusyKey（"conv-<对话id>"，
// 见 conversations.go）⇒ 取数字部分当 scope_id；不是这个形状（或 <=0）返回
// 非法 scope ⇒ 调用方拿到空块（不注入）。
func memoryScopeOfChatSession(sessionID string) db.MemoryScope {
	if !strings.HasPrefix(sessionID, "conv-") {
		return db.MemoryScope{}
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(sessionID, "conv-"), 10, 64)
	if err != nil || id <= 0 {
		return db.MemoryScope{}
	}
	return MemoryScopeForSession(id)
}

// memoryBlockForToolSet 是给工具集用的记忆块取数（ToolSet 可能有 memory DB）。
func memoryBlockForToolSet(ts *ToolSet, taskID int64) string {
	if ts == nil || ts.memoryDB == nil {
		return ""
	}
	return MemoryPromptBlock(ts.memoryDB, memoryScopeOfTaskID(taskID))
}

// rememberTool 返回「追加一条记忆」的工具。
func (t *ToolSet) rememberTool() actool.CoreTool {
	return t.writeExpTool("record_memory",
		"追加一条记忆（append-only：只追加新版本，不覆盖旧版本）。\n"+
			"什么时候写：总结出一条以后还会用到的结论（偏好、教训、已确认的事实）时。\n"+
			"topic_key 必填（同一主题的多版本共用它）；title 必填；summary / kind / rule_ref 可选。\n"+
			"⚠️规则本体不要写在这里：rule_ref 只是指向提示词里规则的**引用名**，索引本身不存放可执行规则。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic_key": map[string]any{"type": "string", "description": "主题键；同一主题的多版本共用它。"},
				"kind":      map[string]any{"type": "string", "description": "类别（如 preference / fact / lesson）。"},
				"title":     map[string]any{"type": "string", "description": "一句话标题。"},
				"summary":   map[string]any{"type": "string", "description": "摘要（一行）。"},
				"rule_ref":  map[string]any{"type": "string", "description": "规则提示词的引用名；索引只指向它，不放规则本体。"},
			},
			"required": []any{"topic_key", "title"},
		},
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.memoryDB == nil {
				return actool.Text("记忆层未启用"), nil
			}
			scope := memoryScopeOfTaskID(t.taskID)
			if !scope.Valid() {
				return actool.Text("记忆作用域不可用（缺少任务上下文）"), nil
			}
			var a struct {
				TopicKey string `json:"topic_key"`
				Kind     string `json:"kind"`
				Title    string `json:"title"`
				Summary  string `json:"summary"`
				RuleRef  string `json:"rule_ref"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数不合法: " + err.Error()), nil
			}
			id, err := AppendMemoryNote(t.memoryDB, scope, a.TopicKey, a.Kind, a.Title, a.Summary, a.RuleRef)
			if err != nil {
				if errors.Is(err, db.ErrMemoryIndexFull) {
					// W2（方案 v10 §6）：上限命中必须响亮——错误信息里带上「该做什么」
					// 的固定提示（代码写死，不由模型生成）。
					return actool.Errorf("记忆追加失败：" + err.Error() + " 记忆索引已达上限，请先 recall 后用 supersede/retire。"), nil
				}
				return actool.Text("记忆追加失败：" + err.Error()), nil
			}
			return actool.Text("memory recorded: " + strconv.FormatInt(id, 10)), nil
		})
}

// recallMemoryTool 返回「检索/导出记忆」的工具。
func (t *ToolSet) recallMemoryTool() actool.CoreTool {
	return t.readExpTool("recall_memory",
		"检索当前任务的记忆。带 query（或 keyword）时走混合检索（全文+向量 RRF 融合，返回最相关的条目，而非整份索引）；不带参数时导出整个记忆索引（纯文本）。写满被拒时先用它看看现状。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "检索词（自然语言）；给出时走混合检索，只返回相关条目。"},
				"keyword": map[string]any{"type": "string", "description": "同 query（别名）。"},
			},
		},
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.memoryDB == nil {
				return actool.Text("记忆层未启用"), nil
			}
			scope := memoryScopeOfTaskID(t.taskID)
			if !scope.Valid() {
				return actool.Text("记忆作用域不可用（缺少任务上下文）"), nil
			}
			var a struct {
				Query   string `json:"query"`
				Keyword string `json:"keyword"`
			}
			_ = json.Unmarshal(in, &a)
			q := strings.TrimSpace(a.Query)
			if q == "" {
				q = strings.TrimSpace(a.Keyword)
			}
			// W4（方案 v10 §6）：带检索词 ⇒ 混合检索；检索失败/降级/零命中 ⇒ 回退
			// 原纯文本导出路径（行为不为空，recall 永远给出可用上下文）。
			if q != "" {
				if out, err := SearchMemory(t.memoryDB, scope, q); err == nil && strings.TrimSpace(out) != "" {
					return actool.Text(out), nil
				}
			}
			out, err := RecallMemory(t.memoryDB, scope)
			if err != nil {
				return actool.Text("记忆导出失败：" + err.Error()), nil
			}
			if strings.TrimSpace(out) == "" {
				return actool.Text("（记忆索引为空）"), nil
			}
			return actool.Text(out), nil
		})
}
