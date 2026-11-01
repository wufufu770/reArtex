package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 本文件是「对抗复核队列」（SoD 链的下游）的两个 agent 工具：
// redteam 读队列、redteam 结案。
//
// 职责分离链的形状：可疑的工具调用被确定性闸门拦下，写进 needs_review 队列，
// 等一个【证伪者】（redteam）来看。redteam 是 chat 型 agent（role=assistant），
// 没有自己的 ToolSet —— 它的工具全靠 tools 表的绑定 + 域注册表注入（见
// server/assembly.go 的 buildDomainReg / ToolResolve）。
//
// 两条硬约束（结构性的，不是约定俗成的）：
//
//  1. **队列访问器是宿主注入的**（SodReviewStore），不是 ToolSet 上的 store。
//     原因：对抗复核队列是【平台级】的表，不是某个任务的任务上下文；而 redteam
//     拿到的实例来自服务端的注册表 ToolSet（探索库为 nil），从那里取不到任何
//     任务级句柄。注入点放宿主（server），agent 包只认这个窄接口。
//     访问器为 nil 时两个工具都优雅降级（中文错误，不 panic）——工具 handler 跑在
//     harness 自己的 goroutine 上，panic 会带走整个进程，服务端的 recover() 够不着。
//
//  2. **结案状态是白名单**：只允许 resolved / dismissed 两个终态。
//     「模型说什么就写什么」⇒ 队列的状态机就废了（db 层虽然也会拒，但那时已经
//     多了一次模型往返与一次误导性的报错）。白名单取值直接引用 db 的状态机常量，
//     免得两处各写一份字符串、日后漂移。

// SodReviewStoreAPI 是对抗复核队列的最小访问面。
//
// 为什么方法名与 db.DB 的（ListAdversarialReviewPending / ResolveAdversarialReview）
// 刻意不同：agent 包不该依赖 db 的具体表形状，服务端用适配器桥接两侧。
type SodReviewStoreAPI interface {
	// ListPending 列出待处理项。taskID > 0 只列该任务，<= 0 列全部；
	// limit <= 0 表示不设上限（由实现方取默认值）。
	ListPending(taskID int64, limit int) ([]*db.AdversarialReviewItem, error)
	// Resolve 结案：by 是署名，resolution 是结论，status 是终态。
	Resolve(id int64, by, resolution, status string) error
}

// SodReviewStore 是宿主注入的「对抗复核队列」访问器。
//
// nil ⇒ 这两个工具不可用（返回中文错误，不崩）。宿主（server）在装配时注入一个
// 覆盖 db.DB 的适配器。
var SodReviewStore SodReviewStoreAPI

// sodReviewDefaultLimit 是读队列的默认条数：够 redteam 看清一批待办，又不至于
// 把整个队列塞进上下文。
const sodReviewDefaultLimit = 20

// sodReviewMaxLimit 是读队列的硬顶。队列可以很长，但模型一次只看这么多——
// 再多就是「把可执行产物塞进上下文」（反模式 8）。
const sodReviewMaxLimit = 100

// sodReviewDefaultActor 是拿不到调用者身份时的兜底署名。
//
// 为什么需要兜底：redteam 是 chat 型 agent，它拿到的工具实例来自服务端的注册表
// ToolSet（worker 为空、成员身份未注入），两个字段都拿不到 ⇒ 署名必须有个值，
// 否则队列里留下一条没有责任人的结案记录。
const sodReviewDefaultActor = "redteam"

// sodReviewClosableStatuses 是「结案」允许的状态白名单。
//
// 只收两个终态：pending / claimed 是中间态，由结案动作取代，不是结案结果。
// 取值引用 db 的状态机常量（db/sod.go），避免这里另写一份字符串日后漂移。
var sodReviewClosableStatuses = map[string]bool{
	db.SoDReviewResolved:  true,
	db.SoDReviewDismissed: true,
}

// sodReviewListSchema 是 list_adversarial_review_pending 的参数 schema。
//
// 抽成包级常量：工具实现与工具目录（toolcatalog.go 的 ToolSeed）必须用同一份
// schema，否则「模型看到的参数」与「代码里实际读的参数」会漂移。
var sodReviewListSchema = obj(map[string]any{
	"task_id": intp("只看某个任务（0 = 全部）"),
	"limit":   intp("最多返回多少条（默认 20，上限 100）"),
})

// sodReviewSubmitSchema 是 submit_adversarial_review_result 的参数 schema。
var sodReviewSubmitSchema = obj(map[string]any{
	"id": intp("要结案的复核项 id（list_adversarial_review_pending 返回的 id）"),
	"resolution": str("结案结论：查清了什么 / 为什么判定无需追查（必填）"),
	"status": map[string]any{
		"type":        "string",
		"enum":        []string{db.SoDReviewResolved, db.SoDReviewDismissed},
		"description": "结案状态：resolved=已查清并给出结论；dismissed=判定无需追查",
	},
}, "id", "resolution", "status")

// listAdversarialReviewPending 让 redteam 读待复核队列。
//
// 只读：它不改动任何状态，只是把「哪些工具调用被闸门拦下待查」摆到证伪者面前。
func (t *ToolSet) listAdversarialReviewPending() actool.CoreTool {
	return readTool(
		"list_adversarial_review_pending",
		"读【对抗复核队列】（needs_review）：职责分离链把可疑的工具调用拦在这里，等你（证伪者）判断。\n"+
			"每条含：来源 agent、工具名、载荷预览、异常标记与原因码、原始结构化意图。\n"+
			"参数：task_id 只看某个任务（0 = 全部）；limit 最多返回多少条（默认 20，上限 100）。\n"+
			"只读，不改任何状态。",
		sodReviewListSchema,
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if SodReviewStore == nil {
				return actool.Errorf("对抗复核队列当前不可用"), nil
			}
			var a struct {
				TaskID int64 `json:"task_id"`
				Limit  int   `json:"limit"`
			}
			if len(in) > 0 {
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf("参数不合法: " + err.Error()), nil
				}
			}
			limit := a.Limit
			if limit <= 0 {
				limit = sodReviewDefaultLimit
			}
			if limit > sodReviewMaxLimit {
				limit = sodReviewMaxLimit
			}
			items, err := SodReviewStore.ListPending(a.TaskID, limit)
			if err != nil {
				return actool.Errorf("读取对抗复核队列失败: " + err.Error()), nil
			}
			b, err := json.Marshal(items)
			if err != nil {
				return actool.Errorf("读取对抗复核队列失败: " + err.Error()), nil
			}
			return actool.Text(string(b)), nil
		},
	)
}

// submitAdversarialReviewResult 让 redteam 结案一条复核项。
//
// 写的是队列自己的状态（结案记录），不是平台共享状态机 ⇒ 能力面是「顾问自己的
// 建议通道」，redteam 的 DenyWrite 挡不住它。
func (t *ToolSet) submitAdversarialReviewResult() actool.CoreTool {
	return writeTool(
		"submit_adversarial_review_result",
		"结案一条【对抗复核队列】里的复核项：给出结论与依据，把它从待查队列里清掉。\n"+
			"id 来自 list_adversarial_review_pending 的返回。\n"+
			"status 只能二选一：resolved=已查清并给出结论；dismissed=判定无需追查。\n"+
			"resolution 是结案依据（必填）：查清了什么 / 为什么判定无需追查。",
		sodReviewSubmitSchema,
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if SodReviewStore == nil {
				return actool.Errorf("对抗复核队列当前不可用"), nil
			}
			var a struct {
				ID         int64  `json:"id"`
				Resolution string `json:"resolution"`
				Status     string `json:"status"`
			}
			if len(in) > 0 {
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf("参数不合法: " + err.Error()), nil
				}
			}
			if a.ID <= 0 {
				return actool.Errorf("复核项 id 不合法：必须给出要结案的那条复核项的 id"), nil
			}
			resolution := strings.TrimSpace(a.Resolution)
			if resolution == "" {
				return actool.Errorf("结案依据不能为空：说明查清了什么 / 为什么判定无需追查"), nil
			}
			status := strings.TrimSpace(a.Status)
			if !sodReviewClosableStatuses[status] {
				return actool.Errorf(fmt.Sprintf("结案状态只能是 %s 或 %s，收到 %q",
					db.SoDReviewResolved, db.SoDReviewDismissed, a.Status)), nil
			}
			if err := SodReviewStore.Resolve(a.ID, t.sodReviewActor(), resolution, status); err != nil {
				if errors.Is(err, db.ErrSoDReviewNotFound) {
					return actool.Errorf("复核项不存在或已被结案：请重新读队列确认 id"), nil
				}
				return actool.Errorf("结案对抗复核项失败: " + err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已结案复核项 %d（状态 %s）", a.ID, status)), nil
		},
	)
}

// sodReviewActor 返回结案署名（by）：谁做的这个结论。
//
// 优先用成员身份（memberAgentKey）——那是【调用者】的身份，与提案配额同一口径；
// 其次用执行器名（worker）。两者都拿不到时（redteam 走服务端注册表拿到的实例，
// 两个字段都是空的）退到常量署名：队列里不留没有责任人的结案记录。
func (t *ToolSet) sodReviewActor() string {
	if t != nil {
		if k := strings.TrimSpace(t.memberAgentKey); k != "" {
			return k
		}
		if w := strings.TrimSpace(t.worker); w != "" {
			return w
		}
	}
	return sodReviewDefaultActor
}
