package agent

import (
	"context"
	"encoding/json"
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

// MemoryScopeForTask 返回「任务大厅」作用域的记忆 scope。
func MemoryScopeForTask(taskID int64) db.MemoryScope {
	return db.MemoryScope{Type: db.MemoryScopeTaskHall, ID: taskID}
}

// MemoryScopeForSession 返回「会话」作用域的记忆 scope。
func MemoryScopeForSession(sessionID int64) db.MemoryScope {
	return db.MemoryScope{Type: db.MemoryScopeChat, ID: sessionID}
}

// MemoryPromptBlock 渲染指定 scope 的记忆块，供拼进 agent 上下文。
//
// 返回空串表示没有记忆 ⇒ 调用方不要塞一个空标题进提示词。
// 读失败**不报错**：记忆是辅助，取不到就当没有（不能因为记忆层故障让整个 agent 挂掉）。
func MemoryPromptBlock(pg *db.DB, scope db.MemoryScope) string {
	if pg == nil || !scope.Valid() {
		return ""
	}
	entries, err := pg.ListMemoryEntries(scope, db.MemoryListOptions{Limit: 50})
	if err != nil || len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(memoryPromptHeading)
	b.WriteString("\n")
	for _, e := range entries {
		if e == nil {
			continue
		}
		topic := e.TopicKey
		if topic == "" {
			topic = "topic"
		}
		title := e.Title
		if title == "" {
			title = e.SummaryLine
		}
		fmt.Fprintf(&b, "- [%s] %s", topic, title)
		if e.SummaryLine != "" && e.SummaryLine != title {
			fmt.Fprintf(&b, " — %s", e.SummaryLine)
		}
		if e.RuleRef != "" {
			fmt.Fprintf(&b, "（规则：%s）", e.RuleRef)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// AppendMemoryNote 追加一条记忆（append-only）。
//
// 同一主题重复写 ⇒ 调用方用 SupersedeMemoryEntry 关掉旧版本；这里只管追加。
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
	e, err := pg.AppendMemoryEntry(scope, db.MemoryEntryInput{
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

// memoryScopeOfTaskID 把任务号翻译成记忆 scope（供工具内部使用）。
func memoryScopeOfTaskID(taskID int64) db.MemoryScope {
	if taskID <= 0 {
		return db.MemoryScope{}
	}
	return MemoryScopeForTask(taskID)
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
				return actool.Text("记忆追加失败：" + err.Error()), nil
			}
			return actool.Text("memory recorded: " + strconv.FormatInt(id, 10)), nil
		})
}

// recallMemoryTool 返回「导出记忆索引」的工具。
func (t *ToolSet) recallMemoryTool() actool.CoreTool {
	return t.readExpTool("recall_memory",
		"导出当前任务的记忆索引（纯文本）。",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.memoryDB == nil {
				return actool.Text("记忆层未启用"), nil
			}
			scope := memoryScopeOfTaskID(t.taskID)
			if !scope.Valid() {
				return actool.Text("记忆作用域不可用（缺少任务上下文）"), nil
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
