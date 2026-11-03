package db

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 记忆层（方案 B）访问层的验收用例。
//
// 这些用例钉住的是**承诺**，不是实现细节：
//   - 幂等 DDL（重复应用不报错）；
//   - append-only + 版本化：追加产生**新行**，旧行的内容一个字都不改；
//   - 取代：旧行被闭合有效区间（valid_to 非空）但**仍然可读**；
//   - scope 过滤是强制的：未 scope 的读写被 typed error 拒绝；
//   - 索引装不下可执行规则：kind 只有 note/link，rule_ref 只是指针；
//   - 硬上限是**响亮拒绝**（ErrMemoryIndexFull），不是静默丢弃；
//   - 纯文本导出是当前索引的可读形状。
//
// ⚠️ 绝不指向生产库或交付库：连接串由 ARTEX_PG_DSN 派生库名重建，派生库不可用
// 时**跳过**（不是失败）。

// memoryTestBaseDSN 记住进程启动时的 ARTEX_PG_DSN（包初始化时读一次）。
var memoryTestBaseDSN = os.Getenv("ARTEX_PG_DSN")

// memoryTestDerivedDB 是 internal/testdb.Setup("db") 派生的库名。
//
// 命名规则在 internal/testdb/testdb.go（"artex_test_" + 包名）。这里只用它来
// **重建连接串**，不重建库 —— 重建会把本包自己的库删掉。
const memoryTestDerivedDB = "artex_test_db"

// memoryTestScopeSeq 让每个用例拿到互不相同的 scope 键。
var memoryTestScopeSeq atomic.Int64

// memoryTestDSN 取测试库连接串。
//
// ⚠️ 为什么不用 testDSN：db/pgdsn_env_test.go 的用例会清空 DSN 缓存并把环境
// 变量还原成空，而本文件按文件名排在它后面 ⇒ testDSN 拿不到连接串，用例会被
// 静默跳过。这里按 testdb 的派生名自己重建连接串，并 ping 确认库确实在。
//
// ⚠️ 为什么**不**在用例里调 testdb.Setup("db")：派生库名是固定的
// （artex_test_db），重建会把这个包里其他用例正在用的库删掉。
//
// ⚠️ 为什么不动环境变量：db.DSN() 读入即从进程环境抹掉 DSN（防 agent 敲
// env 读出数据库口令），本文件直接用重建出的连接串，不走 DSN()。
func memoryTestDSN(t *testing.T) string {
	t.Helper()
	if memoryTestBaseDSN == "" {
		t.Skip("没有数据库配置（ARTEX_PG_DSN 未设置）—— 跳过")
	}
	u, err := url.Parse(memoryTestBaseDSN)
	if err != nil {
		t.Skipf("数据库配置无法解析（%v）—— 跳过", err)
	}
	u.Path = "/" + memoryTestDerivedDB
	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", err)
	}
	perr := conn.Ping()
	_ = conn.Close()
	if perr != nil {
		t.Skipf("派生测试库不可用（%v）—— 跳过", perr)
	}
	return u.String()
}

// memoryOpen 打开测试库并确保记忆 DDL 已应用。
func memoryOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(memoryTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureMemorySchema(); err != nil {
		t.Fatalf("应用记忆 schema 失败: %v", err)
	}
	return d
}

// memoryTestScope 给用例一个全新的 scope 键。
//
// 记忆层**不解释** scope_id 的含义（它是调用方的会话键），所以合成一个不撞车
// 的键即可，不必真造会话行。
func memoryTestScope(t *testing.T) MemoryScope {
	t.Helper()
	return MemoryScope{
		Type: MemoryScopeTaskHall,
		ID:   time.Now().UnixNano() + memoryTestScopeSeq.Add(1),
	}
}

// TestEnsureMemorySchemaIsIdempotent 断言记忆的 DDL 幂等。
func TestEnsureMemorySchemaIsIdempotent(t *testing.T) {
	d := memoryOpen(t)

	// EnsureMemorySchema 的第二次调用走 sync.Map 快路径、碰不到 SQL ⇒ 这里
	// 显式再应用一遍 DDL，验证的是 SQL 本身的幂等性。
	if _, err := d.Exec(memorySchemaSQL); err != nil {
		t.Fatalf("重复应用记忆 DDL 失败: %v", err)
	}
	for _, q := range []string{
		`SELECT to_regclass('public.memory_index')`,
		`SELECT to_regclass('public.memory_topic')`,
		`SELECT to_regclass('public.memory_write_log')`,
		`SELECT to_regclass('public.memory_index_current')`,
	} {
		var name *string
		if err := d.QueryRow(q).Scan(&name); err != nil {
			t.Fatalf("query %s: %v", q, err)
		}
		if name == nil {
			t.Fatalf("%s 不存在", q)
		}
	}
}

// TestMemoryAppendCreatesNewVersion 断言追加产生**新版本**而不是改旧行。
func TestMemoryAppendCreatesNewVersion(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	first, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey:    "asset-surface",
		Kind:        MemoryKindNote,
		Title:       "第一版标题",
		SummaryLine: "第一版摘要",
		Body:        "第一版正文",
		WrittenBy:   "worker#1",
	})
	if err != nil {
		t.Fatalf("追加第一版失败: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("第一版版本号应为 1，实际 %d", first.Version)
	}

	second, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey:    "asset-surface",
		Kind:        MemoryKindNote,
		Title:       "第二版标题",
		SummaryLine: "第二版摘要",
		Body:        "第二版正文",
		WrittenBy:   "worker#2",
	})
	if err != nil {
		t.Fatalf("追加第二版失败: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("追加没有产生新行：旧行被就地改写了")
	}
	if second.Version != 2 {
		t.Fatalf("第二版版本号应为 2，实际 %d", second.Version)
	}
	if second.SupersedesID == nil || *second.SupersedesID != first.ID {
		t.Fatalf("第二版应指向它取代的旧行: %v", second.SupersedesID)
	}

	// 旧行的内容一个字都没改（仍是第一版），只是有效区间被闭合。
	old, err := d.GetMemoryEntry(first.ID)
	if err != nil {
		t.Fatalf("读旧行失败: %v", err)
	}
	if old.Title != "第一版标题" || old.SummaryLine != "第一版摘要" {
		t.Fatalf("旧行内容被改写了: %q / %q", old.Title, old.SummaryLine)
	}
	if old.ValidTo == nil {
		t.Fatal("旧行的有效区间没被闭合")
	}
	if old.SupersededBy == nil || *old.SupersededBy != second.ID {
		t.Fatalf("旧行应指向取代它的新行: %v", old.SupersededBy)
	}

	// 当前索引里同一主题只剩最新一版。
	current, err := d.ListMemoryEntries(scope, MemoryListOptions{})
	if err != nil {
		t.Fatalf("列当前索引失败: %v", err)
	}
	if len(current) != 1 {
		t.Fatalf("当前索引应只剩一版，实际 %d 条", len(current))
	}
	if current[0].ID != second.ID || current[0].Title != "第二版标题" {
		t.Fatalf("当前索引应是第二版: %d / %q", current[0].ID, current[0].Title)
	}

	// 历史版本仍然可读（内容没删，只是闭合了区间）。
	all, err := d.ListMemoryEntries(scope, MemoryListOptions{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("列全部版本失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("全部版本应为 2 条，实际 %d 条", len(all))
	}
}

// TestMemorySupersedeKeepsOldEntryReadable 断言取代会把旧条目标记掉但**保留可读**。
func TestMemorySupersedeKeepsOldEntryReadable(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	old, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey:    "waf-bypass",
		Kind:        MemoryKindNote,
		Title:       "旧结论",
		SummaryLine: "旧摘要",
		Body:        "旧正文",
		WrittenBy:   "planner",
	})
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	next, err := d.SupersedeMemoryEntry(scope, old.ID, MemoryEntryInput{
		Title:       "新结论",
		SummaryLine: "新摘要",
		Body:        "新正文",
		WrittenBy:   "planner",
	})
	if err != nil {
		t.Fatalf("取代失败: %v", err)
	}
	if next.SupersedesID == nil || *next.SupersedesID != old.ID {
		t.Fatalf("新行应指向它取代的旧行: %v", next.SupersedesID)
	}
	if next.TopicKey != old.TopicKey {
		t.Fatalf("取代必须沿用原主题键: %q vs %q", next.TopicKey, old.TopicKey)
	}
	if next.Version != old.Version+1 {
		t.Fatalf("新版本号应为 %d，实际 %d", old.Version+1, next.Version)
	}

	// 旧条目被标记为被取代，但仍然读得到、内容没变。
	readBack, err := d.GetMemoryEntry(old.ID)
	if err != nil {
		t.Fatalf("被取代的条目应仍可读: %v", err)
	}
	if readBack.Title != "旧结论" {
		t.Fatalf("被取代的条目内容被改写了: %q", readBack.Title)
	}
	if readBack.ValidTo == nil {
		t.Fatal("被取代的条目应闭合有效区间")
	}
	if readBack.SupersededBy == nil || *readBack.SupersededBy != next.ID {
		t.Fatalf("被取代的条目应指向新行: %v", readBack.SupersededBy)
	}
}

// TestMemoryTopicKeyRequiredOnCreate 断言「新建主题」必须给主题键，而「取代」允许
// 不给（主题键从被取代的那一行继承）。
func TestMemoryTopicKeyRequiredOnCreate(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	if _, err := d.AppendMemoryEntry(scope, MemoryEntryInput{Title: "无主题键"}); err == nil {
		t.Fatal("新建主题时缺主题键应被拒绝")
	}

	old, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "inherit-topic", Title: "旧版",
	})
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	next, err := d.SupersedeMemoryEntry(scope, old.ID, MemoryEntryInput{Title: "新版"})
	if err != nil {
		t.Fatalf("取代时不给主题键应被接受: %v", err)
	}
	if next.TopicKey != old.TopicKey {
		t.Fatalf("取代应继承原主题键: %q vs %q", next.TopicKey, old.TopicKey)
	}
}

// TestMemoryScopeFiltering 断言 scope 过滤是强制的。
func TestMemoryScopeFiltering(t *testing.T) {
	d := memoryOpen(t)
	scopeA := memoryTestScope(t)
	scopeB := memoryTestScope(t)
	scopeB.Type = MemoryScopeChat

	if _, err := d.AppendMemoryEntry(scopeA, MemoryEntryInput{
		TopicKey: "a-topic", Title: "A 的记忆", WrittenBy: "worker",
	}); err != nil {
		t.Fatalf("写 scope A 失败: %v", err)
	}
	if _, err := d.AppendMemoryEntry(scopeB, MemoryEntryInput{
		TopicKey: "b-topic", Title: "B 的记忆", WrittenBy: "worker",
	}); err != nil {
		t.Fatalf("写 scope B 失败: %v", err)
	}

	listA, err := d.ListMemoryEntries(scopeA, MemoryListOptions{})
	if err != nil {
		t.Fatalf("列 scope A 失败: %v", err)
	}
	if len(listA) != 1 || listA[0].TopicKey != "a-topic" {
		t.Fatalf("scope A 应只有自己的一条，实际 %d 条", len(listA))
	}
	listB, err := d.ListMemoryEntries(scopeB, MemoryListOptions{})
	if err != nil {
		t.Fatalf("列 scope B 失败: %v", err)
	}
	if len(listB) != 1 || listB[0].TopicKey != "b-topic" {
		t.Fatalf("scope B 应只有自己的一条，实际 %d 条", len(listB))
	}

	// 未 scope 的读写一律 typed error 拒绝。
	if _, err := d.AppendMemoryEntry(MemoryScope{}, MemoryEntryInput{TopicKey: "x"}); !errors.Is(err, ErrMemoryScopeRequired) {
		t.Fatalf("未 scope 的写入应被拒绝，实际 %v", err)
	}
	if _, err := d.ListMemoryEntries(MemoryScope{Type: MemoryScopeChat, ID: 0}, MemoryListOptions{}); !errors.Is(err, ErrMemoryScopeRequired) {
		t.Fatalf("未 scope 的读取应被拒绝，实际 %v", err)
	}
	if _, err := d.ExportMemoryIndex(MemoryScope{Type: "not-a-scope", ID: 1}); !errors.Is(err, ErrMemoryScopeRequired) {
		t.Fatalf("非法 scope 类型应被拒绝，实际 %v", err)
	}
}

// TestMemoryIndexHoldsNoRules 断言索引装不下可执行规则。
func TestMemoryIndexHoldsNoRules(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	_, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "rule-ish", Kind: "rule", Title: "一条规则",
	})
	if !errors.Is(err, ErrMemoryKindInvalid) {
		t.Fatalf("kind='rule' 应被拒绝，实际 %v", err)
	}

	_, err = d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "rule-text", RuleRef: "禁止在生产环境执行任何命令",
	})
	if !errors.Is(err, ErrMemoryRuleRefInvalid) {
		t.Fatalf("rule_ref 装规则正文应被拒绝，实际 %v", err)
	}

	// 指针形状的 rule_ref 允许（规则本体住在 agent_prompts）。
	if _, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "rule-pointer", RuleRef: "agent_prompts:42", Title: "指向规则",
	}); err != nil {
		t.Fatalf("合法的 rule_ref 指针应被接受: %v", err)
	}
}

// TestMemoryExportIsPlainText 断言纯文本导出的内容。
func TestMemoryExportIsPlainText(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	if _, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey:         "sqli-on-login",
		Kind:             MemoryKindNote,
		Title:            "登录点存在拼接查询",
		SummaryLine:      "参数 user 直接拼进 SQL",
		Tags:             []string{"sqli", "login"},
		RuleRef:          "agent_prompts:7",
		SourceSessionIDs: []int64{11, 12},
		WrittenBy:        "worker#3",
		Body:             "详细复现步骤……",
	}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	text, err := d.ExportMemoryIndex(scope)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	for _, want := range []string{
		"# 记忆索引",
		scope.String(),
		"sqli-on-login",
		"登录点存在拼接查询",
		"参数 user 直接拼进 SQL",
		"sqli, login",
		"agent_prompts:7",
		"worker#3",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("导出缺 %q:\n%s", want, text)
		}
	}
}

// TestMemoryWriteLogChains 断言写入账是按 scope 串起来的哈希链。
func TestMemoryWriteLogChains(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	first, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "chain-topic", Title: "第一条", WrittenBy: "worker",
	})
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if _, err := d.SupersedeMemoryEntry(scope, first.ID, MemoryEntryInput{
		Title: "第二条", WrittenBy: "worker",
	}); err != nil {
		t.Fatalf("取代失败: %v", err)
	}

	logs, err := d.ListMemoryWriteLog(scope, 50)
	if err != nil {
		t.Fatalf("列写入账失败: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("写入账应有 2 行，实际 %d 行", len(logs))
	}
	if logs[0].Op != MemoryOpAppend || logs[1].Op != MemoryOpSupersede {
		t.Fatalf("写入账的 op 不对: %q / %q", logs[0].Op, logs[1].Op)
	}
	if logs[0].NewHash == "" || logs[1].PrevHash != logs[0].NewHash {
		t.Fatalf("哈希链断了: %q vs %q", logs[1].PrevHash, logs[0].NewHash)
	}
}

// TestMemoryIndexCapRejectsLoudly 断言索引硬上限是**响亮拒绝**而不是静默丢弃。
func TestMemoryIndexCapRejectsLoudly(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	// 每条都写到接近索引行长上限 ⇒ 字节上限必然先到（条目数上限是 150）。
	filler := strings.Repeat("长", MemoryTitleMaxLen)
	var capped bool
	for i := 0; i < MemoryIndexMaxEntries+10; i++ {
		_, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
			TopicKey:    "cap-topic-" + strconv.Itoa(i),
			Title:       filler,
			SummaryLine: filler,
			WrittenBy:   "worker",
		})
		if errors.Is(err, ErrMemoryIndexFull) {
			capped = true
			break
		}
		if err != nil {
			t.Fatalf("第 %d 次追加失败: %v", i, err)
		}
	}
	if !capped {
		t.Fatal("索引达到硬上限时没有拒绝写入（应当响亮拒绝，不是静默丢弃）")
	}

	// 被拒的那次写入没有落库：当前条目数不超过上限。
	current, err := d.ListMemoryEntries(scope, MemoryListOptions{Limit: MemoryIndexMaxEntries + 10})
	if err != nil {
		t.Fatalf("列当前索引失败: %v", err)
	}
	if len(current) > MemoryIndexMaxEntries {
		t.Fatalf("当前索引条数超限: %d", len(current))
	}
}

// TestMemoryIndexIsAppendOnlyAtDBLevel 断言数据库层挡住了就地改写与删除。
func TestMemoryIndexIsAppendOnlyAtDBLevel(t *testing.T) {
	d := memoryOpen(t)
	scope := memoryTestScope(t)

	entry, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
		TopicKey: "guarded", Title: "原标题", Body: "原正文", WrittenBy: "worker",
	})
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	if _, err := d.Exec(`UPDATE memory_index SET title='改写' WHERE id=$1`, entry.ID); err == nil {
		t.Fatal("append-only 触发器没生效：内容列被就地改写了")
	}
	if _, err := d.Exec(`DELETE FROM memory_index WHERE id=$1`, entry.ID); err == nil {
		t.Fatal("append-only 触发器没生效：条目被删了")
	}

	// 正文被改过 ⇒ 读的时候必须报摘要不一致，而不是把被改过的正文喂给模型。
	if _, err := d.Exec(`UPDATE memory_topic SET body='被改过的正文' WHERE entry_id=$1`, entry.ID); err != nil {
		t.Fatalf("改写正文失败: %v", err)
	}
	if _, err := d.ReadMemoryTopic(entry.ID); !errors.Is(err, ErrMemoryTopicDigestMismatch) {
		t.Fatalf("正文被改过后应报摘要不一致，实际 %v", err)
	}
}
