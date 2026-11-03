package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件是【任务级进度回执】（progress receipt）。
//
// 为什么需要它：worker 的对话上下文会被自动压缩（compaction），压缩只保留「边界
// 之后」的消息 ⇒ 压缩前模型已经做完的步骤，在压缩后的上下文里【不存在】。若进度
// 只活在对话里，模型会在压缩后重做已经做过的步骤（重复探测、重复写图）；而外部
// 观察者（UI / 规划者）看到的是「这个任务好像挂了」—— 长任务在 UI 上像挂了，是
// 用户反馈的头号。
//
// 回执落在【activity 表】：这张表已经存在、已经按 exploration_id 分区、已经按 id
// 排序、已经能被 SSE 推给前端 ⇒ 不需要新表、不需要新 DDL。
//
// ⚠️ activity.kind 是自由文本列（TEXT，无 CHECK 约束）⇒ kind='progress' 不需要动
// db/schema.sql（那是 RESERVED 文件）。
//
// 契约（两条工具 + 一条运行期回执）：
//   - record_progress：模型写一条回执（summary 必填；detail / intent_id / step 可选）。
//   - list_progress_receipts：读回执（压缩之后模型靠它想起「哪几步已经做完了」）。
//   - recordRunProgressReceipt：运行结束时由【运行循环】落一条确定性的回执（写回
//     了什么），不依赖模型有没有想起来调工具。
//
// ⚠️ 阻塞不走这里：阻塞（做不下去、等外部条件）走 intercept_pending 那条路
// （确定性闸门记录 + 规划者唤醒）。把阻塞塞进回执，会让回执的语义从「已完成」变成
// 「状态机」—— 而状态机归确定性层，模型说了不算。

// ProgressActivityKind 是进度回执在 activity.kind 里的取值。
//
// 唯一真相源：写它、读它、筛它都用这一个常量，避免「写用 progress、筛用 progess」
// 这种静默失配（筛不到 = 回执看起来丢了）。
const ProgressActivityKind = "progress"

// progressReceiptDefaultLimit / progressReceiptMaxLimit 是读回执的默认条数与硬顶。
//
// 为什么要有硬顶：回执是模型自己写的，行数不可控 ⇒ 不设顶的话一次读回执就能把整段
// 上下文塞满（反模式：把可执行产物塞进上下文）。
const (
	progressReceiptDefaultLimit = 20
	progressReceiptMaxLimit     = 100
	progressReceiptPageSize     = 200
)

// progressReceiptMeta 是回执行里带的结构化字段（activity.metadata，JSONB）。
//
// 为什么只放这两个：metadata 是回执与 UI 之间唯一的结构化通道，放「阶段」与「归属」
// 就够反查了；正文一律进 summary/detail（人读的部分）。
type progressReceiptMeta struct {
	IntentID int64  `json:"intent_id,omitempty"`
	Step     string `json:"step,omitempty"`
}

// progressReceiptView 是回执给模型看的投影（只留有用的字段）。
//
// 为什么不用 db.Activity 直接返回：activity 行带 tool / tool_use_id / token 计数等
// 一堆与回执无关的列，直接序列化会把噪声塞进模型上下文。
type progressReceiptView struct {
	ID        int64     `json:"id"`
	IntentID  int64     `json:"intent_id,omitempty"`
	Worker    string    `json:"worker,omitempty"`
	Step      string    `json:"step,omitempty"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// progressReceiptSchema 是 record_progress 的参数 schema。
//
// 抽成包级常量：工具实现与工具目录（toolcatalog.go 的 ToolSeed）必须用同一份
// schema，否则「模型看到的参数」与「代码里实际读的参数」会漂移（与 sod_review 的
// 两条 schema 同一处置）。
var progressReceiptSchema = obj(map[string]any{
	"summary":   str("一句话说明这一步做成了什么（必填）"),
	"detail":    str("可选：这一步的细节（命令、关键输出、还差什么）"),
	"intent_id": idp("可选：这一步归属的意图 id；省略则用本次运行的意图"),
	"step":      str("可选：这一步的短标签（如 recon / exploit / verify），便于回查"),
}, "summary")

// progressReceiptListSchema 是 list_progress_receipts 的参数 schema。
var progressReceiptListSchema = obj(map[string]any{
	"intent_id": idp("可选：只看某条意图的回执；省略则看本任务全部"),
	"limit":     intp("最多返回多少条（默认 20，上限 100）"),
})

// recordProgress 让 agent 写一条【任务级进度回执】。
//
// 写的是 activity 表（append-only），不改任何状态机、不改归属、不扩范围 ⇒ 与
// 「追加 fact」同族的 scope 内结构化写（分层见 agent/approval_policy.go 的
// tier2Progress）。
func (t *ToolSet) recordProgress() actool.CoreTool {
	return t.writeExpTool("record_progress",
		"写一条【任务级进度回执】：用一句话说明这一步做成了什么，落到本任务的执行轨迹里（append-only，不改任何状态）。\n"+
			"为什么要写：对话上下文会被自动压缩，压缩前的内容不会保留 ⇒ 进度只留在对话里就会被压缩抹掉。回执落在库里，进度因此不随上下文消失。\n"+
			"什么时候写：**每完成一个可复述的步骤就写一条**（发现了什么、验证到哪一步、还剩什么），不要攒到最后。\n"+
			"summary 必填（一句话说清做成了什么）；detail 可选（细节 / 命令 / 关键输出）；intent_id 可选（省略=本次运行的意图）；step 可选（短标签，便于回查）。\n"+
			"⚠️回执不是状态机：它只说明「做到了什么」，不改变任何东西。遇到做不下去（被拦、被拒、等外部条件）不要写回执 —— 那条路是阻断上报，不是这里。",
		progressReceiptSchema,
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Summary  string          `json:"summary"`
				Detail   string          `json:"detail"`
				Step     string          `json:"step"`
				IntentID json.RawMessage `json:"intent_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数不合法: " + err.Error()), nil
			}
			summary := strings.TrimSpace(a.Summary)
			if summary == "" {
				return actool.Errorf("summary 不能为空：请用一句话说明这一步做成了什么"), nil
			}
			intent := pid(a.IntentID)
			if intent <= 0 {
				intent = t.ownerNode
			}
			if intent > 0 {
				// 归属必须是本任务的意图：intent_id 会落到 activity.node_id（外键指向
				// exploration_nodes）⇒ 越界的 id 由外键挡下，但错误信息会很难懂，
				// 所以先自己判一次并给一句人话。
				node, err := t.ts.GetNode(intent)
				if err != nil || node == nil || node.Kind != db.KindIntent {
					return actool.Errorf("intent_id 必须是本任务的意图 id；不确定就省略这个参数"), nil
				}
			}
			meta, err := json.Marshal(progressReceiptMeta{IntentID: intent, Step: strings.TrimSpace(a.Step)})
			if err != nil {
				return actool.Errorf("回执写入失败: " + err.Error()), nil
			}
			act := db.Activity{
				Worker:   t.worker,
				Kind:     ProgressActivityKind,
				Summary:  summary,
				Detail:   strings.TrimSpace(a.Detail),
				Metadata: meta,
			}
			if intent > 0 {
				act.NodeID = &intent
			}
			id, err := t.ts.AppendActivity(act)
			if err != nil {
				return actool.Errorf("回执写入失败: " + err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("progress recorded: %d", id)), nil
		})
}

// listProgressReceipts 让 agent 读回【本任务的进度回执】。
//
// 压缩之后，模型靠它想起「哪几步已经做完了」—— 这是回执存在的另一半理由（另一半
// 是给 UI / 规划者看的持久轨迹）。
//
// ⚠️ 分层：它【不在】Tier 1 只读清单里。Tier 1 的启用被「读面范围隔离验收测试」
// （agent/tier1_scope_gate_test.go）绑定，而那道测试的用例表从
// agent.Tier1ReadOnlyTools 派生 ⇒ 加进 Tier 1 必须同时补一条范围隔离用例。
// 本工具的读面天然按 exploration_id 隔离（只看本任务的行），补用例的成本很低，
// 但在补上之前它落在【默认 Tier 3】（fail closed：只有被证明安全的工具才自动放行）。
func (t *ToolSet) listProgressReceipts() actool.CoreTool {
	return t.readExpTool("list_progress_receipts",
		"读本任务的【进度回执】（按时间倒序，最新的在前）：每条含 id / 归属意图 / 阶段标签 / 一句话摘要。\n"+
			"用途：上下文被压缩之后，先读它，确认哪几步已经做完，别重复做。\n"+
			"intent_id 可选（只看某条意图的回执；省略=本任务全部）；limit 最多返回多少条（默认 20，上限 100）。\n"+
			"只读，不改任何状态。",
		progressReceiptListSchema,
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Limit    int             `json:"limit"`
			}
			if len(in) > 0 {
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf("参数不合法: " + err.Error()), nil
				}
			}
			limit := a.Limit
			if limit <= 0 {
				limit = progressReceiptDefaultLimit
			}
			if limit > progressReceiptMaxLimit {
				limit = progressReceiptMaxLimit
			}
			var nodeID *int64
			if id := pid(a.IntentID); id > 0 {
				nodeID = &id
			}
			rows, err := collectProgressReceipts(t.ts, nodeID, limit)
			if err != nil {
				return actool.Errorf("读取进度回执失败: " + err.Error()), nil
			}
			return jsonResult(map[string]any{"count": len(rows), "receipts": rows})
		})
}

// collectProgressReceipts 回溯收集最近的 progress 回执（返回时按时间升序）。
//
// 为什么不用一次 ActivityPage 就完事：那一页是「最近 N 条 activity」，而 progress
// 只是其中一种 kind ⇒ 最近的一页里可能一条 progress 都没有（全是 tool_use/
// tool_result）⇒ 读到的会是空列表，而回执明明存在。于是从最新一页往回翻，收够
// want 条就停 —— 有界（每页 progressReceiptPageSize 条）且不会漏掉更早的回执。
func collectProgressReceipts(ts *db.ExplorationStore, nodeID *int64, want int) ([]progressReceiptView, error) {
	if ts == nil || want <= 0 {
		return nil, nil
	}
	filter := db.ActivitySessionFilter{NodeID: nodeID}
	before := int64(0)
	newest := []progressReceiptView{}
	for len(newest) < want {
		page, _, err := ts.ActivityPage(filter, before, progressReceiptPageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for i := len(page) - 1; i >= 0; i-- {
			if page[i].Kind != ProgressActivityKind {
				continue
			}
			newest = append(newest, progressReceiptViewOf(page[i]))
			if len(newest) == want {
				break
			}
		}
		if len(newest) == want {
			break
		}
		before = page[0].ID // 下一页从这一页最早那条之前继续（id 严格递减 ⇒ 不会死循环）
	}
	// newest 是【新→旧】，翻成【旧→新】返回（与库里 id 升序一致）。
	out := make([]progressReceiptView, 0, len(newest))
	for i := len(newest) - 1; i >= 0; i-- {
		out = append(out, newest[i])
	}
	return out, nil
}

// progressReceiptViewOf 把一条 activity 行投影成回执视图。
func progressReceiptViewOf(a db.Activity) progressReceiptView {
	v := progressReceiptView{
		ID:        a.ID,
		Summary:   a.Summary,
		CreatedAt: a.CreatedAt,
		Worker:    a.Worker,
	}
	if a.NodeID != nil {
		v.IntentID = *a.NodeID
	}
	var meta progressReceiptMeta
	if len(a.Metadata) > 0 && json.Unmarshal(a.Metadata, &meta) == nil {
		v.Step = meta.Step
		if v.IntentID == 0 {
			v.IntentID = meta.IntentID
		}
	}
	return v
}

// recordRunProgressReceipt 在【运行结束】时落一条确定性的进度回执。
//
// 为什么在运行循环里落，而不是只靠模型调 record_progress：
//   - 模型可能一次都没调（收尾轮被预算打断、或模型压根没想到要调）；
//   - UI 要的是「这个任务还活着 + 它干了什么」的稳定信号；
//   - 压缩之后模型看不到自己做过什么，而这条回执在库里（可回读）。
//
// 内容来自 WriteCounts（确定性事实，不是模型自述）⇒ 符合「模型只提议、确定性层
// 裁决」：回执记的是「确实写回了什么」，不是「模型说自己做了什么」。
//
// 没写回任何东西的运行不落回执：空回执只会污染轨迹（还占 UI 的位置）。
func recordRunProgressReceipt(ts *db.ExplorationStore, worker string, intentID int64, w WriteCounts) {
	if ts == nil || w.Total() == 0 {
		return
	}
	meta, err := json.Marshal(progressReceiptMeta{IntentID: intentID, Step: "run-summary"})
	if err != nil {
		return
	}
	act := db.Activity{
		Worker:   worker,
		Kind:     ProgressActivityKind,
		Summary:  "本次执行写回：" + w.String(),
		Metadata: meta,
	}
	if intentID > 0 {
		act.NodeID = &intentID
	}
	_, _ = ts.AppendActivity(act)
}
