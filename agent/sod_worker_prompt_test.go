package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// 本文件是 SoD（职责分离）链的【回归保护】：钉住「链的存在对模型完全不可见」。
//
// 链落在工具执行入口上（PreToolUse hook + 执行器装饰器），与提示词**毫无关系**：
// 模型的措辞、模型的自我描述、模型的「请管理员给我放行」都不会影响闸门的裁决 ——
// 裁决只看规整器产出的结构化字段与宿主查到的实时状态。所以这条链**可以**在不改
// 提示词一个字节的前提下装上去。
//
// 两条断言各守一半：
//   - TestSoDWorkerPromptUnchanged：正文一个字节都不能变（正文变了 = 既有 worker
//     的行为变了，而这条链不允许顺带改别人的行为）。
//   - TestSoDWorkerPromptMentionsNoChain：正文里**不能出现**任何链的字样。
//     模型一旦读到「意图规整」「确定性闸门」这类词，就可能推断出存在一道可被
//     说服的关卡，并试图说服它 —— 那等于把「模型只提议」变成「模型参与裁决」。
//     链必须对模型**完全不存在**，而不是「存在但别提」。
//
// 为什么放在 agent 包内（而不是外部测试包）：断言的对象是 agent 包的
// BuiltinPromptBody / Tier1ReadOnlyTools / BuiltinToolSeeds 这几个**包内真相源**。
// 在包外另抄一份清单，等于出现第二个真相源，此后「清单说安全」与「现实真的安全」
// 可以各说各话，而没有任何东西会变红。

const (
	// workerPromptGoldenBytes 是 worker 内置提示词正文（段 [A]，即可编辑部分）
	// 的黄金字节数。
	//
	// 为什么同时钉字节数与摘要：字节数是最直观的「变没变」的读数，摘要钉住
	// 「变的是哪几个字节」。两个一起看，才分得清「改了一个错别字」与「重写了
	// 整段铁律」—— 后者是要审的，前者也要审，只是严重程度不同。
	workerPromptGoldenBytes = 2687

	// workerPromptGoldenDigest 是同一份正文的黄金 SHA-256。
	//
	// ⚠️ 这份摘要是**既有 agent 的行为基线**：改一个字节就是改既有 worker 的行为，
	// 属于有意识、有记录的动作，绝不能是「顺手 tidy 一下」的结果。SoD 链落地**不**
	// 构成改提示词的理由 —— 链在工具执行入口上，与提示词无关。
	//
	// 真要改 worker 提示词时，先人工确认「这次改动与 SoD 无关」，再更新这里的
	// 摘要；不许为了让测试变绿而更新它。
	workerPromptGoldenDigest = "80fbb1c610dd57e266c20b9145793339eee540b809f6db8f5336261b45879f08"
)

// TestSoDWorkerPromptUnchanged 断言 worker 的内置提示词正文一个字节都没变。
//
// 这是「worker 学不到链的存在」的**守门人**：链完全活在工具执行入口里，与提示词
// 无关 ⇒ 装链不该动提示词。测试变红 ⇒ 有人在装链的同时动了提示词。
func TestSoDWorkerPromptUnchanged(t *testing.T) {
	body, ok := BuiltinPromptBody("worker")
	if !ok {
		t.Fatalf("取不到 worker 的内置提示词正文：BuiltinPromptBody(\"worker\") 返回 ok=false —— 种子目录里没有 worker 这一项")
	}

	if got := len(body); got != workerPromptGoldenBytes {
		t.Errorf("worker 提示词正文长度变了：got = %d 字节, want = %d 字节（多出来的字节就是提示词被改过的证据；SoD 链不构成改提示词的理由）", got, workerPromptGoldenBytes)
	}

	sum := sha256.Sum256([]byte(body))
	if got := hex.EncodeToString(sum[:]); got != workerPromptGoldenDigest {
		t.Errorf("worker 提示词正文摘要变了：got = %s, want = %s（既有 agent 的行为变更不在本次范围内；若确实与 SoD 无关，需人工确认后更新黄金摘要）", got, workerPromptGoldenDigest)
	}
}

// TestSoDWorkerPromptMentionsNoChain 断言 worker 的提示词里没有任何链的字样。
//
// 为什么是「一个都不能出现」而不是「不要出现得太显眼」：模型读到「意图规整」
// 「确定性闸门」这类词，就可能推断出「存在一道关卡，而关卡是有可能被说服的」，
// 于是去说服它 —— 那是让模型参与裁决，「模型只提议、确定性层裁决」当场破掉。
// 链必须对模型**完全不存在**。
//
// 为什么用大小写敏感的朴素子串匹配：这些字样全是平台内部术语，模型看不到它们
// 就不会出现；而大小写不敏感会把正常的英文单词（"sod" 之类）也算成命中，
// 那会让这条断言变成噪声而不是闸门。
func TestSoDWorkerPromptMentionsNoChain(t *testing.T) {
	body, ok := BuiltinPromptBody("worker")
	if !ok {
		t.Fatalf("取不到 worker 的内置提示词正文：BuiltinPromptBody(\"worker\") 返回 ok=false —— 种子目录里没有 worker 这一项")
	}

	forbidden := []string{
		// 规整器（链的第一段）的 agent key 与它写进意图里的字段名。
		"intent_normalizer",
		// 同一个 agent key 的连字符写法（文件名 / 阶段名，见
		// agent/prompts_files.go 的 promptKeyAliases）—— 泄漏这一种同样等于泄漏链。
		"intent-normalizer",
		// 链本身的简称与闸门规格的文件名 / 别名。
		"SoD",
		"policy-gate",
		"policy_gate",
		// 闸门与规整环节的中文名。
		"意图规整",
		"确定性闸门",
		// 一次性执行凭证。
		"执行凭证",
		// 规整器产出的结构化字段名。
		"structured_intent",
		// 链的留痕表 / 队列名：出现它们等于告诉模型「你的每次调用都会被记账」。
		"sod_decisions",
		"adversarial_review",
	}

	var hits []string
	for _, w := range forbidden {
		if strings.Contains(body, w) {
			hits = append(hits, w)
		}
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		t.Errorf("worker 提示词正文里出现了 %d 处链的字样：%v —— 模型一旦读到它们就可能推断出存在一道可被说服的关卡；链必须对模型完全不存在（而不是「存在但别提」）", len(hits), hits)
	}
}

// notGatedPlatformExpansion 是方案 v3 缺陷 B1 扩容进放行清单的平台工具白名单，
// 每项带理由。两类：
//   - 只读：读探索图 / 执行链路 / 会话输出缓冲 —— 与 Tier 1 同级的安全只读，
//     只是尚未逐个补 tier1_scope_gate 的范围隔离用例；
//   - 结构化记账：record_fact / record_progress 的写目标**固定在任务探索图与
//     回执表**（不触宿主 FS、不出网、不碰裁决），写侧边界由工具实现自身保证。
//
// 新的平台工具要进放行清单，必须先加进这份白名单并写明理由 —— 下面的断言会
// 拦住「顺手把一个未评审的平台工具塞进放行清单」的回归。Bash/Write/MCP/自定义
// 工具永远不该出现在这里（它们不在这个断言的平台目录里，但那份清单仍然要过
// 人工评审）。
var notGatedPlatformExpansion = map[string]string{
	"get_worker_trace":         "只读：worker 转录读取",
	"get_worker_output":        "只读：worker 最终产出读取",
	"search_all_worker_traces": "只读：跨 worker 转录检索",
	"node_detail":              "只读：探索图节点详情",
	"expand_digest":            "只读：摘要展开",
	"list_goals":               "只读：目标清单",
	"recall_memory":            "只读：历史记忆检索",
	"list_companies":           "只读：公司范围清单",
	"record_fact":              "结构化记账：写任务探索图（B1/B2）",
	"record_progress":          "结构化记账：写任务回执（B1/B2）",
	"list_progress_receipts":   "只读：回执清单",
}

// TestSoDNotGatedToolsAreTier1ReadOnly 断言放行清单里的**平台工具**必须是
// Tier 1 只读工具，或在 notGatedPlatformExpansion 白名单里有逐项评审记录。
//
// 背景：internal/sodpolicy.NotGatedTools 是 SoD 链的【放行清单】—— 清单里的工具
// 不进链、不需要执行凭证，也就不受 scope / 熔断 / 额度 / tier2 四道检查管束。
// 清单以外的**全部**工具都进链（这样未登记的新工具默认被管住）。
//
// 危险的方向只有一侧：放行清单里出现一个**未评审**的平台工具。Tier 1 是
// 「已证明范围隔离的只读工具」—— 它进放行清单是安全的；一个既不在 Tier 1、
// 又没有白名单评审记录的平台工具进放行清单，等于给它开了一扇默认不设防的门。
//
// 为什么「平台工具」「Tier 1」「扩容白名单」是三个集合：
//   - Tier 1（agent.Tier1ReadOnlyTools）是平台侧「安全只读」的真相源，清单在
//     agent/approval_policy.go。**Tier 1 的五工具必须始终是放行清单的子集**
//     —— 这是本断言钉住的第一条不变量（回归方向：有人从清单里删 Tier 1）。
//   - 扩容白名单（notGatedPlatformExpansion）是 B1 扩容的评审记录；平台工具
//     进放行清单必须出现在这里。
//   - 本地只读（Read / LS / Glob）与纯记账（TodoWrite / Sleep）不是平台工具，
//     本就不需要范围隔离的证明。
//
// internal/sodpolicy 是叶子包，**不得** import agent（否则 agent → internal/sod
// → internal/sodpolicy → agent 成环）⇒ 放行清单只能是它自己的副本 ⇒ 一致性
// 只能在这里断言。
//
// 为什么平台工具取 agent.BuiltinToolSeeds()（agent 侧的平台工具目录）：它是
// agent 侧能看到的全部平台工具；服务端注册的平台工具（如 get_finding_traffic）
// 不在这个目录里 ⇒ 本断言只覆盖 agent 侧的那一半。这一半已经包含最危险的一类
// （改平台状态的写工具：record_fact / report_finding / insert_assets / add_intent
// …），record_fact / record_progress 已按 B1/B2 的评审口径进了白名单；其余写工具
// （report_finding / insert_assets / add_intent …）一旦出现在放行清单里，这条
// 断言立刻变红。
func TestSoDNotGatedToolsAreTier1ReadOnly(t *testing.T) {
	tier1 := make(map[string]bool, len(Tier1ReadOnlyTools))
	for _, k := range Tier1ReadOnlyTools {
		tier1[k] = true
		// 不变量①：Tier 1 五工具必须始终被放行清单覆盖（删掉 = 只读工具被
		// 拉去烧规整器，B1 要修的原缺陷反向复发）。
		if !slices.Contains(sodpolicy.NotGatedTools, k) {
			t.Fatalf("Tier 1 只读工具 %q 必须在放行清单（internal/sodpolicy.NotGatedTools）里", k)
		}
	}

	platform := make(map[string]bool)
	for _, s := range BuiltinToolSeeds() {
		platform[s.Key] = true
	}

	var offenders []string
	var localOnly []string
	for _, k := range sodpolicy.NotGatedTools {
		switch {
		case tier1[k]:
			// 平台只读且已证明范围隔离：放行清单允许它豁免。
		case platform[k] && notGatedPlatformExpansion[k] != "":
			// 平台工具但在扩容白名单里有逐项评审记录：允许。
		case platform[k]:
			// 平台工具却不在 Tier 1、也不在白名单：未评审，拦下。
			offenders = append(offenders, k)
		default:
			// 既不在 Tier 1、也不是平台工具：本地只读（Read / LS / Glob）或纯记账
			// （TodoWrite / Sleep）。它们不碰平台状态，按设计不进 Tier 1。
			localOnly = append(localOnly, k)
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("放行清单里的平台工具 %v 既不在 Tier 1 只读清单、也不在扩容白名单（notGatedPlatformExpansion）—— 未评审的平台工具不得默认不设防：修法二选一：把该工具从放行清单里拿掉（让它进链），或先完成安全评审（只读，或写目标固定在任务图内）后把理由写进 notGatedPlatformExpansion", offenders)
	}

	sort.Strings(localOnly)
	t.Logf("放行清单里不属平台工具面的条目（本地只读 / 纯记账，按设计不进 Tier 1）：%v", localOnly)
}
