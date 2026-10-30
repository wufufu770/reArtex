package llmrec

import (
	"errors"
	"fmt"
	"strings"
)

// GroupQuotaMarker 标记「平台自己的群组预算拒绝」。
//
// 为什么需要它：群组预算用尽与供应商额度耗尽是**两件相反的事** ——
// 前者只要停派发（新任务不再认领），后者要切走 profile（换一个供应商继续跑）。
// 若不区分，平台自己的拒绝消息会被供应商额度 markers 命中，于是当前 profile 被
// 标成 quota_exhausted 并切走：供应商其实还好好的，别的任务也因此失去它
// （task_llm_profiles 被污染），群组预算的语义也被埋进了供应商额度里。
//
// 依赖方向：agent 已经 import llmrec，llmrec 不 import agent —— 无环。
const GroupQuotaMarker = "[[group-quota]]"

// GroupQuotaRefusalCopy 是群组预算拒绝时展示给用户的中文文案。
//
// 文案必须**避开**供应商额度 markers 的字面（「额度不足」「额度已用尽」「欠费」
// 「余额不足」…）：一旦撞车，即便没有标记也会被判成供应商额度耗尽。故此处用
// 「预算」而非「额度」措辞 —— 与 GroupQuotaMarker 双保险：标记兜住语义，文案
// 兜住字面。
const GroupQuotaRefusalCopy = "群组预算不足或不可用，已中止本次模型请求"

// IsGroupQuotaMessage 判断一条错误文案是不是平台自己的群组预算拒绝。
func IsGroupQuotaMessage(message string) bool {
	return strings.Contains(message, GroupQuotaMarker)
}

// GroupQuotaRefusal 构造群组预算拒绝错误：带上标记，且文案固定为不会撞车的那份。
//
// 把它做成构造函数而不是让调用方自己拼，是为了让「拒绝消息必然带标记、必然用
// 安全文案」成为**结构上的不变量**，而不是每个调用点的自觉。cause 可为 nil。
func GroupQuotaRefusal(cause error) error {
	if cause == nil {
		return errors.New(GroupQuotaMarker + " " + GroupQuotaRefusalCopy)
	}
	return fmt.Errorf("%s %s: %w", GroupQuotaMarker, GroupQuotaRefusalCopy, cause)
}
