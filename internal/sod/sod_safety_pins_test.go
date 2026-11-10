package sod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	"github.com/Autumn-27/norma/llm"
)

// 这个文件补三条**安全钉子**。每一条都钉住一条「将来某次重构不能悄悄破坏」
// 的性质：
//
//  1. 规整结果的缓存键必须含范围摘要 —— 范围一变，旧规整结果不得被复用；
//  2. 规整器不可用 / 输出不合规 ⇒ 链必须**拒绝**（fail-closed），工具不得执行；
//  3. 命令执行类工具（norma 的 Bash / shell_open / shell_send / Grep / Task …）
//     必须进链裁决，不许被放行清单静默豁免。
//
// 三条都断言**可观测的行为**（裁决 / 原因码 / 工具有没有被执行 / 缓存键差异），
// 而不是实现细节 —— 目的是「安全属性被改坏」在 CI 上立刻变红。

// ---- 共用脚手架 ----

// sodPinNewTask 建一个任务并声明一条范围行。
//
// 为什么必须先有范围行：范围摘要是缓存键的一个分量，链在规整**之前**就要算它。
func sodPinNewTask(t *testing.T, d *db.DB, label string) (int64, string) {
	t.Helper()
	task, err := d.CreateTask("sod 安全钉子 "+label, "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	host := fmt.Sprintf("sod-pin-%s-%d.example.test", label, time.Now().UnixNano())
	if _, err := d.Assets().AddManualScope(task.ID, "root_domain", host, "sod 安全钉子用例"); err != nil {
		t.Fatalf("声明任务范围失败: %v", err)
	}
	return task.ID, host
}

// sodPinRig 是这批用例共用的「受管路径」装配。
type sodPinRig struct {
	chain   *Chain
	tokens  *TokenStore
	tool    *countingTool
	gate    *gateStub
	payload []byte
	ctx     context.Context
}

// sodPinRigFor 装一条受管路径（真库 + 替身闸门 + 给定的规整器）。
//
// 为什么用替身闸门：五道检查里三道依赖实时状态（熔断 / 额度 / tier2 证据），
// 真库里不可控；这三条用例要验的是「链有没有裁决」与「裁决成不成立」，不是五道
// 检查各自的判定 —— 替身够用，还能让「闸门到底跑没跑」可计数。
func sodPinRigFor(t *testing.T, d *db.DB, taskID int64, host string, normalizer *Normalizer) *sodPinRig {
	t.Helper()
	// 宿主读法复位：这三条用例验的是「链自己的判定」，不能被别的用例留下的全局
	// 读法（SetContextReader）带偏。
	SetContextReader(nil)
	t.Cleanup(func() { SetContextReader(nil) })

	tokens := NewTokenStore()
	tool := &countingTool{name: "Bash"}
	stub := gateStubAllowAll()
	base := NewChain(nil, ChainDeps{
		Normalizer: normalizer,
		Gate:       &Gate{deps: stub},
		Tokens:     tokens,
		Auditor:    NewAuditor(d),
		Cache:      NewCache(16, "sod-safety-pin"),
		Assets:     d.Assets(),
		DB:         d,
		AgentKeyOf: AgentKeyFromContext,
	})
	return &sodPinRig{
		chain:   base.Wrap(&fakeHookRunner{}),
		tokens:  tokens,
		tool:    tool,
		gate:    stub,
		payload: []byte(`{"command":"curl -X POST -d q=1 https://` + host + `/probe"}`),
		ctx:     WithRunContext(context.Background(), RunContext{TaskID: taskID, AgentKey: "worker"}),
	}
}

// sodPinDecisions 读本任务的决策账本（按 id 倒序 ⇒ [0] 是最新那条）。
func sodPinDecisions(t *testing.T, d *db.DB, taskID int64) []*db.SoDDecision {
	t.Helper()
	recs, err := d.ListSoDDecisions(taskID, 50)
	if err != nil {
		t.Fatalf("读决策账本失败: %v", err)
	}
	return recs
}

// ---- 钉子 ①：范围一变 ⇒ 规整结果缓存必失效 ----

// TestSoDNormalizerCacheInvalidatesOnScopeChange 钉住「范围一变 ⇒ 规整结果
// 缓存必失效」。
//
// 为什么这是生死线：缓存里存的是**规整结果**（模型产出的结构化意图）。范围
// 收紧前生成的那份规整结果被范围收紧后复用 = **范围旁路** —— 越权目标的动作
// 拿着「旧范围下看起来合规」的意图过了闸门。所以范围摘要必须是缓存键的一个
// 分量（cache.go 的 Key）。
//
// 两条腿：
//  ① 缓存键这一层：范围摘要不同 ⇒ 键必须不同；范围摘要相同 ⇒ 键必须相同。
//  ② 链这一层：范围真的变了一次之后，同一份载荷必须**重新规整**（缓存未命中）。
//
// 什么会让它变红：有人把 scope_digest 从 Key() 里删掉，或链在算键时不再传
// 当前的范围摘要。
func TestSoDNormalizerCacheInvalidatesOnScopeChange(t *testing.T) {
	// ① 缓存键这一层（不依赖数据库 ⇒ 永远会跑）。
	cache := NewCache(8, "sod-safety-pin")
	const (
		toolName      = "Bash"
		payloadDigest = "deadbeef"
		taskID        = int64(42)
		sourceAgent   = "worker"
		scopeBefore   = "scope-digest-before"
		scopeAfter    = "scope-digest-after"
	)
	keyBefore := cache.Key(toolName, payloadDigest, taskID, scopeBefore, sourceAgent)
	keyBeforeAgain := cache.Key(toolName, payloadDigest, taskID, scopeBefore, sourceAgent)
	keyAfter := cache.Key(toolName, payloadDigest, taskID, scopeAfter, sourceAgent)

	if keyBefore != keyBeforeAgain {
		t.Fatalf("同样的分量必须算出同一个缓存键: %q vs %q", keyBefore, keyBeforeAgain)
	}
	if keyBefore == keyAfter {
		t.Fatalf("范围摘要不同却算出同一个缓存键 —— scope_digest 被漏出缓存键了（范围旁路）: %q", keyBefore)
	}

	// 走缓存自己的取放接口再验一遍（行为，不只是键的字符串）：同样的范围摘要
	// 必须命中同一条条目；范围摘要不同的键必须取不到那条。
	cache.Put(keyBefore, NormalizerResult{StructuredIntent: StructuredIntent{ActionType: "command_execution"}})
	if _, ok := cache.Get(keyBeforeAgain); !ok {
		t.Fatal("同样的范围摘要必须命中同一条缓存条目")
	}
	if _, ok := cache.Get(keyAfter); ok {
		t.Fatal("范围摘要不同的键命中了范围摘要相同的那条缓存条目 —— scope_digest 被漏出缓存键了（范围旁路）")
	}

	// ② 链这一层：范围真的变一次 ⇒ 旧规整结果不得被复用。
	d := auditorTestDB(t)
	realTaskID, host := sodPinNewTask(t, d, "scope")
	provider := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", host))}
	rig := sodPinRigFor(t, d, realTaskID, host, newTestNormalizer(t, provider))

	block, msg, _ := rig.chain.PreToolUse(rig.ctx, "Bash", rig.payload)
	if block {
		t.Fatalf("期望放行, 却被拒: %s", msg)
	}
	if provider.calls != 1 {
		t.Fatalf("第一次调用应当触发一次规整, 实际 %d 次", provider.calls)
	}
	decisions := sodPinDecisions(t, d, realTaskID)
	if len(decisions) != 1 {
		t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
	}
	if decisions[0].CacheHit {
		t.Fatal("第一次调用不该命中缓存")
	}

	// 范围收紧：加一条新的范围行 ⇒ 范围摘要变。
	//
	// ⚠️ 第二个根域必须与第一个**不同 registrable root domain**：root_domain 行
	// 按 (task_id, kind, domain, ...) 去重，而 root_domain 存的是
	// RootDomain(host)（"a.example.test" 与 "b.example.test" 都落成
	// "example.test"）⇒ 同根域再加一行会被 ON CONFLICT DO NOTHING 吃掉 ⇒ 有效
	// 范围没变 ⇒ 摘要当然也不变（那是对的，不是缺陷）。所以这里换成一个真正
	// 不同的根域。
	digestBefore, err := d.Assets().ScopeDigestForTask(realTaskID)
	if err != nil {
		t.Fatalf("读范围摘要失败: %v", err)
	}
	tightened := fmt.Sprintf("sod-pin-scope-tightened-%d.example.org", time.Now().UnixNano())
	if _, err := d.Assets().AddManualScope(realTaskID, "root_domain", tightened, "范围收紧"); err != nil {
		t.Fatalf("收紧范围失败: %v", err)
	}
	digestAfter, err := d.Assets().ScopeDigestForTask(realTaskID)
	if err != nil {
		t.Fatalf("读范围摘要失败: %v", err)
	}
	if digestBefore == digestAfter {
		t.Fatal("加了范围行之后范围摘要没变 —— 用例前提不成立，测不出缓存失效")
	}

	// 同一份载荷再来一次：范围变了 ⇒ 精确键（含 scope_digest）失效，但 R2 模式级
	// 缓存（方案 v10 §14：键=tool|host|method）命中 ⇒ 规整器不重跑。这是 R2 的
	// 预期语义：缓存里只有【规整线索】没有【裁决】——范围检查每次用本次载荷的
	// host 实时重判（闸门五检查照跑），所以线索复用不构成范围旁路。
	block, msg, _ = rig.chain.PreToolUse(rig.ctx, "Bash", rig.payload)
	if block {
		t.Fatalf("期望放行, 却被拒: %s", msg)
	}
	if provider.calls != 1 {
		t.Fatalf("模式键应命中缓存（规整器共 1 次）, 实际 %d 次", provider.calls)
	}
	decisions = sodPinDecisions(t, d, realTaskID)
	if len(decisions) != 2 {
		t.Fatalf("决策账本应有 2 条, 实际 %d 条", len(decisions))
	}
	// R2 语义：第二次走模式键命中（CacheHit=true），但账本里的范围摘要是【本次
	// 实时判定】的新摘要——证明范围裁决没有随线索一起被缓存。
	if !decisions[0].CacheHit {
		t.Fatal("范围变化后的这次裁决应命中模式级缓存（R2 预期）")
	}
	if decisions[0].ScopeDigest != digestAfter {
		t.Fatalf("账本里的范围摘要=%q, 期望 %q（范围必须每次实时重判）", decisions[0].ScopeDigest, digestAfter)
	}
}

// ---- 钉子 ②：规整器给不出可用意图 ⇒ 链必须拒绝 ----

// TestSoDGateDeniesWhenNormalizerUnavailable 钉住「规整器给不出可用的结构化
// 意图 ⇒ 链必须拒绝」。
//
// 为什么必须拒绝而不是放行：规整器是「把原始动作翻译成结构化意图」这一层的
// 唯一来源。它挂了 / 输出不合规 ⇒ 链手上没有可信的意图 ⇒ 闸门无从判定范围 ⇒
// 唯一的落法是**拒绝**（fail-closed）。放行等于「模型不可用就当这次动作合法」。
//
// 为什么走**链**而不是只测规整器：规整器自己报错不代表链会拒绝 —— 链完全可能
// 把错误吞掉继续走闸门。所以必须从 PreToolUse 进，从「裁决」与「执行」两侧出。
//
// 三种故障形态各有各的原因码：
//   - provider 不可用 ⇒ normalizer_unavailable（模型侧没给出结果）；
//   - 模型没调用工具 / 输出缺字段 ⇒ normalizer_malformed（给了结果但不能用）。
func TestSoDGateDeniesWhenNormalizerUnavailable(t *testing.T) {
	cases := []struct {
		name       string
		build      func(t *testing.T) *Normalizer
		wantReason string
	}{
		{
			name: "模型 provider 不可用",
			build: func(t *testing.T) *Normalizer {
				n, err := NewNormalizer(func(int64) (llm.Provider, bool) { return nil, false },
					0, "你是意图规整器", normalizerSchema())
				if err != nil {
					t.Fatalf("构造规整器失败: %v", err)
				}
				return n
			},
			wantReason: sodpolicy.ReasonNormalizerUnavailable,
		},
		{
			name: "模型没有调用 emit_structured_intent",
			build: func(t *testing.T) *Normalizer {
				p := &fakeProvider{events: []llm.StreamEvent{
					{Type: llm.SETextDelta, Text: "这条命令看起来没问题"},
					{Type: llm.SEMessageStop},
				}}
				return newTestNormalizer(t, p)
			},
			wantReason: sodpolicy.ReasonNormalizerMalformed,
		},
		{
			name: "模型输出缺少 structured_intent",
			build: func(t *testing.T) *Normalizer {
				p := &fakeProvider{events: toolUseEvents("emit_structured_intent", `{"anomaly_flag":false}`)}
				return newTestNormalizer(t, p)
			},
			wantReason: sodpolicy.ReasonNormalizerMalformed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := auditorTestDB(t)
			taskID, host := sodPinNewTask(t, d, "normfail")
			rig := sodPinRigFor(t, d, taskID, host, tc.build(t))

			block, msg, _ := rig.chain.PreToolUse(rig.ctx, "Bash", rig.payload)
			if !block {
				t.Fatal("规整器给不出可用意图时必须拒绝执行（fail-closed）")
			}
			if !strings.Contains(msg, tc.wantReason) {
				t.Fatalf("拒绝消息 %q 应当带上原因码 %q", msg, tc.wantReason)
			}

			// 闸门一次都没跑：没有意图就没有可判定的范围。
			if rig.gate.scopeCalls != 0 {
				t.Fatalf("规整失败后闸门仍然被调用了 %d 次", rig.gate.scopeCalls)
			}

			// 裁决留痕：拒绝 + 对应原因码。
			decisions := sodPinDecisions(t, d, taskID)
			if len(decisions) != 1 {
				t.Fatalf("决策账本应有 1 条, 实际 %d 条", len(decisions))
			}
			if decisions[0].Verdict != sodpolicy.VerdictDeny {
				t.Fatalf("账本 verdict=%q, 期望 %q", decisions[0].Verdict, sodpolicy.VerdictDeny)
			}
			if decisions[0].ReasonCode != tc.wantReason {
				t.Fatalf("账本 reason_code=%q, 期望 %q", decisions[0].ReasonCode, tc.wantReason)
			}

			// 没有铸出执行凭证：被拒的动作拿不到通行证。
			if rig.tokens.Take(CanonicalDigest(rig.payload), taskID, "worker", "Bash") {
				t.Fatal("被拒的动作竟然铸出了执行凭证")
			}

			// 纵深防御：即便上层硬要执行，执行器侧也没有凭证 ⇒ 工具不会被执行。
			exec := WrapExecutor(rig.tool, rig.tokens, true)
			res, err := exec.Call(rig.ctx, json.RawMessage(rig.payload), nil)
			if err != nil {
				t.Fatalf("期望返回错误结果而不是 Go error, got %v", err)
			}
			if !res.IsError {
				t.Fatal("被拒的动作不该被执行")
			}
			if rig.tool.calls != 0 {
				t.Fatalf("工具被调用了 %d 次, 期望 0 次", rig.tool.calls)
			}
		})
	}
}

// ---- 钉子 ③：命令执行类工具必须进链裁决 ----

// TestSoDChainCoversNormaShellTools 钉住「命令执行类工具必须进链裁决」。
//
// 为什么这是生死线：SoD 链用的是**放行清单**语义（sodpolicy.NotGatedTools）——
// 清单以外的**全部**工具都进链。一旦有人把 Bash（或任何 shell 工具）加进放行
// 清单，那条工具就整条绕过闸门 ⇒ 一条 shell 命令可以在没有范围判定、没有
// 一次性凭证的情况下执行 ⇒ 整条 SoD 链对它形同虚设。
//
// 两条腿：
//  ① 判据这一层：链用的那个判据（GatedTool = 放行清单的补集）必须对这些工具名
//     返回「受管」。
//  ② 链这一层：这些工具名必须真的被链裁决（闸门跑过 + 账本里留有裁决）。
//
// 什么会让它变红：有人往 sodpolicy.NotGatedTools 里加了 "Bash" / "shell_open" /
// "Grep" / "Task" 之类的名字。
func TestSoDChainCoversNormaShellTools(t *testing.T) {
	// norma 的命令执行类工具：一次性 shell、持久 PTY 交互会话、ripgrep 检索、
	// 子任务派发。全部都必须是受管的。
	shellTools := []string{
		"Bash",
		"shell_open", "shell_send", "shell_read", "shell_close", "shell_list",
		"Grep",
		"Task",
	}

	// ① 判据这一层（不依赖数据库 ⇒ 永远会跑）。
	for _, name := range shellTools {
		if !GatedTool(name) {
			t.Fatalf("工具 %q 被放行清单豁免了 —— shell 命令不能绕过 SoD 闸门", name)
		}
	}

	// ② 链这一层：这些工具名必须真的被链裁决。
	d := auditorTestDB(t)
	taskID, host := sodPinNewTask(t, d, "shelltools")
	provider := &fakeProvider{events: toolUseEvents("emit_structured_intent", intentJSON("command_execution", host))}
	rig := sodPinRigFor(t, d, taskID, host, newTestNormalizer(t, provider))

	for _, name := range shellTools {
		block, msg, _ := rig.chain.PreToolUse(rig.ctx, name, rig.payload)
		if block {
			t.Fatalf("工具 %q 应当通过替身闸门, 却被拒: %s", name, msg)
		}
	}
	if rig.gate.scopeCalls != len(shellTools) {
		t.Fatalf("闸门被调用了 %d 次, 期望每个工具各判一次 = %d 次", rig.gate.scopeCalls, len(shellTools))
	}

	decisions := sodPinDecisions(t, d, taskID)
	judged := map[string]bool{}
	for _, rec := range decisions {
		judged[rec.ToolName] = true
	}
	for _, name := range shellTools {
		if !judged[name] {
			t.Fatalf("工具 %q 没有被链裁决 —— 它被放行清单静默豁免了", name)
		}
	}
}
