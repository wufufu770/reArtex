// 本文件是记忆层（方案 B）的访问层 —— 与 db/schema_memory.sql 一一对应。
//
// 设计约束（session-groupchat-proposal.md §9 第 6 条 / §10.4 方案 B / §10.5 /
// §11.5.3 / §11.8 / §8 红线）：
//
//	① **append-only + 版本化**：索引行一旦写下就不可就地改写。「内容变了」= 追加
//	   一个新版本行（version+1，supersedes_id 指向旧行），旧行只被「闭合有效区间」
//	   （valid_to 非空）标记为被取代。数据库层有触发器兜底（schema_memory.sql），
//	   这里也**不提供任何改内容的入口**。
//	② **scope 强制**：scope_type/scope_id 不为空 ⇒ 未 scope 的读写一律 typed error
//	   拒绝（§10.5-1 抄 Mem0 的 VALIDATION_001）。
//	③ **索引与正文分离**：索引行（标题 + 一行摘要）常驻，正文在 memory_topic，
//	   按需读（§11.1-1：记忆必须是索引，不要把内容全塞进上下文）。
//	④ **规则不住在索引里**：kind 只有 note/link，没有 rule；规则本体住在
//	   agent_prompts，索引只放指针 rule_ref ⇒ 这里校验 rule_ref 只能是
//	   agent_prompts:<id> 形式的指针，装不下规则正文（§11.5.3-2）。
//	⑤ **索引硬上限 + 截断响亮**：写入超限**拒绝**（ErrMemoryIndexFull），绝不
//	   静默丢弃条目（§10.5-2 / §11.8-4：截断必须响亮）。
//	⑥ **记忆是提示，不是安全边界**：本文件的任何函数都不参与权限判定。读失败只
//	   影响提示内容，不影响任何放行/拒绝（§11.3 冲突 3：记忆可编辑，边界不可编辑）。
//	⑦ **dream 只提 diff**：记忆写入只有「追加新版本 / 取代 / 退役」三种状态迁移，
//	   没有「整体重写索引」的入口（§11.5.3-1：记忆也是摘要，用更好的摘要去修一个
//	   摘要往往不работает ⇒ 索引必须 append-only，dream 只提 diff）。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为 db/memory.go 一个文件，且下面
// 所有访问器共用同一条约束（「append-only、scope 强制、阈值在代码常量、状态解析
// 不出来一律报错让调用方 fail-closed」）。拆成多个文件会把这条约束切成多份副本，
// 而多份副本正是漂移的开始（同 db/sod.go 的说明）。
package db

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed schema_memory.sql
var memorySchemaSQL string

// memorySchemaReady 记录某个连接已经应用过记忆 DDL。
//
// 与 db/sod.go 的 sodSchemaReady 同理：用包级 sync.Map，键是底层 *sql.DB 指针
// —— 每个测试包/实例各连各的库，互不干扰。
var memorySchemaReady sync.Map

// EnsureMemorySchema 应用记忆层的幂等 DDL。
//
// db/schema.sql 由别的单元持有（RESERVED 文件），本单元的 DDL 放在
// db/schema_memory.sql，由这里在首次访问时幂等补齐（与 db/sod.go 的
// EnsureSoDSchema 同款）。schema 属主把内容并进 db/schema.sql 之后，这里重复
// 应用也无副作用。
//
// ⚠️ 本单元**不在 db.Open 里**调用它（db/db.go 不归本单元持有）：懒加载范式保证
// 第一次访问时建表。代价是「进程启动即建表」这一步留给编排任务。
func (d *DB) EnsureMemorySchema() error {
	if _, ok := memorySchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(memorySchemaSQL); err != nil {
		return fmt.Errorf("应用记忆 schema 失败: %w", err)
	}
	memorySchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureMemorySchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureMemorySchema() error { return d.EnsureMemorySchema() }

const (
	// MemoryScopeTaskHall / MemoryScopeChat 是记忆归属的两个 scope 类型。
	//
	// scope 指向【会话】（sessions.kind），不指向 agent（§9 第 6 条：per-agent
	// 记忆 = N 份发散副本 + 写冲突）。本层**不解释** scope_id 的含义（它是调用方
	// 的键），只保证「按 scope 过滤」这条不变式。
	MemoryScopeTaskHall = "task_hall"
	MemoryScopeChat     = "chat"

	// MemoryKindNote / MemoryKindLink 是索引行的两种 kind。
	//
	// **没有 kind='rule'** —— 规则本体住在 agent_prompts，索引只放指针
	//（§11.5.3-2）。这是结构性的：DDL 的 CHECK 也只认这两个取值。
	MemoryKindNote = "note"
	MemoryKindLink = "link"

	// MemoryStateActive / MemoryStateQuarantined 是晋级门（X4，方案 v10 §6 T2）
	// 的两段式披露状态。
	//
	//   active      —— 进入注入块与检索面（披露第一段）；
	//   quarantined —— 隔离态：agent（模型）写入即隔离，host-only 晋级后才可见。
	//
	// 转态**没有就地更新**入口：append-only 触发器把 state 也钉在内容列守卫里，
	// 晋级走 Supersede（追加 active 新版本并闭合旧区间）、退役走 Retire
	//（闭合区间）这两条双时态通道。
	MemoryStateActive      = "active"
	MemoryStateQuarantined = "quarantined"

	// MemoryIndexMaxEntries 是单个 scope 的**当前**索引条目数上限。
	// 超出 ⇒ 写入被拒（响亮拒绝，不是静默截断）。
	MemoryIndexMaxEntries = 150
	// MemoryIndexMaxBytes 是单个 scope 当前索引的纯文本导出字节上限。
	MemoryIndexMaxBytes = 20 * 1024
	// MemoryTitleMaxLen / MemorySummaryMaxLen 是索引行两段文本的长度上限：索引是
	// 常驻上下文，标题与摘要必须短（§10.4 方案 B 的 title ≤120 / summary ≤200）。
	MemoryTitleMaxLen   = 120
	MemorySummaryMaxLen = 200
)

var (
	// ErrMemoryScopeRequired：未 scope 的读写一律拒绝（§10.5-1）。
	ErrMemoryScopeRequired = errors.New("记忆必须带 scope：scope_type 与 scope_id 不能为空")
	// ErrMemoryKindInvalid：kind 只能是 note/link —— 索引装不下可执行规则。
	ErrMemoryKindInvalid = errors.New("记忆 kind 只能是 note 或 link（规则本体住在 agent_prompts，索引只放指针）")
	// ErrMemoryRuleRefInvalid：rule_ref 只能是 agent_prompts:<id> 形式的指针。
	ErrMemoryRuleRefInvalid = errors.New("rule_ref 只能是 agent_prompts:<id> 形式的指针，不能放规则正文")
	// ErrMemoryVersionConflict：同一 scope 内同一主题被并发写入（唯一约束即 CAS）。
	ErrMemoryVersionConflict = errors.New("记忆版本冲突：同一 scope 内同一主题已被并发写入，请重读后重试")
	// ErrMemoryIndexFull：索引达到硬上限，写入被拒（响亮拒绝，绝不静默丢弃）。
	ErrMemoryIndexFull = errors.New("记忆索引已达硬上限，写入被拒：请先退役或压缩旧条目")
	// ErrMemoryEntryNotFound：条目不存在。
	ErrMemoryEntryNotFound = errors.New("记忆条目不存在")
	// ErrMemoryScopeMismatch：条目不属于给定的 scope。
	ErrMemoryScopeMismatch = errors.New("记忆条目不属于该 scope")
	// ErrMemoryTopicDigestMismatch：正文与它自己的摘要不一致 ⇒ 内容被改过。
	ErrMemoryTopicDigestMismatch = errors.New("记忆正文与内容摘要不一致（内容被改过）")
)

// memoryRuleRefPattern 约束 rule_ref 的形状：只允许指向 agent_prompts 的主键。
//
// 为什么用形状而不是内容判定：「索引不得装下可执行规则」这条不变式需要一个
// **结构上**的保证 —— 一个指针装不下一段规则正文，所以格式约束比关键词黑名单更
// 可靠（黑名单挡不住「先写下规则、以后再拆成两段」这种绕法）。
var memoryRuleRefPattern = regexp.MustCompile(`^agent_prompts:[0-9]+$`)

// MemoryScope 定位一段记忆的归属范围。
type MemoryScope struct {
	// Type 是 MemoryScopeTaskHall 或 MemoryScopeChat。
	Type string
	// ID 是会话键（调用方的键，本层不解释它的含义）。
	ID int64
}

// Valid 报告这个 scope 能否用于读写。
func (s MemoryScope) Valid() bool {
	if s.ID <= 0 {
		return false
	}
	return s.Type == MemoryScopeTaskHall || s.Type == MemoryScopeChat
}

// String 是 scope 的稳定短表示（写进纯文本导出与日志）。
func (s MemoryScope) String() string { return s.Type + ":" + strconv.FormatInt(s.ID, 10) }

// MemoryEntryInput 是「要追加的一版记忆」的输入。
//
// 为什么与 MemoryEntry 分开：MemoryEntry 是**读出来**的行（带 id / 时间戳 / 有效
// 区间），追加语义下这些都不该由调用方给 —— id 与时间戳由数据库决定，调用方能给
// 的只有内容本身。
type MemoryEntryInput struct {
	// TopicKey 是主题键：同一 scope 内一个主题的多版本共用它。
	TopicKey string
	// Kind 是 note 或 link（空 ⇒ note）。
	Kind string
	// Title / SummaryLine 是索引行：一行标题 + 一行摘要。
	Title       string
	SummaryLine string
	// Tags 是标签（可空）。
	Tags []string
	// RuleRef 是规则指针（agent_prompts:<id>），空 ⇒ 不指向规则。
	RuleRef string
	// SourceSessionIDs 记来源会话。
	SourceSessionIDs []int64
	// WrittenBy 记「谁写的」（审计用，不是归属，见 §9 第 6 条）。
	WrittenBy string
	// State 是两段式披露状态（X4）：active / quarantined。空 ⇒ 由写入入口的
	// 默认值决定（agent 的 Checked 路径默认隔离，host 通道默认 active）。
	State string
	// Body 是正文（markdown），落在 memory_topic，按需读。
	Body string
	// BodyDigest 可选：给出时必须等于 sha256(Body)，否则说明调用方算的是另一份
	// 内容 ⇒ 报错，而不是各写各的。
	BodyDigest string
}

// MemoryEntry 是记忆索引的一行（一个版本）。
//
// 被取代的行**仍然可读**（valid_to 非空、superseded_by 指向取代它的那一行）：
// 内容一个字都没改，历史随时能查（§10.2：用「关闭区间」代替删除来表达取代）。
type MemoryEntry struct {
	ID               int64
	ScopeType        string
	ScopeID          int64
	TopicKey         string
	Version          int
	Kind             string
	Title            string
	SummaryLine      string
	Tags             []string
	RuleRef          string
	SourceSessionIDs []int64
	WrittenBy        string
	ContentDigest    string
	// State 是两段式披露状态（X4）：active / quarantined。晋级走 Supersede
	// 通道（同主题追加 active 新版本），退役走 Retire（闭合区间）。
	State        string
	SupersedesID *int64
	ValidFrom    time.Time
	ValidTo      *time.Time
	SupersededBy *int64
	CreatedAt    time.Time
}

// MemoryWriteLogEntry 是写入账的一行（只追加的账本）。
type MemoryWriteLogEntry struct {
	ID        int64
	ScopeType string
	ScopeID   int64
	Op        string
	Actor     string
	EntryID   *int64
	PrevHash  string
	NewHash   string
	Detail    json.RawMessage
	CreatedAt time.Time
}

// 写入账的 op 取值（与 DDL 的 CHECK 镜像）。
const (
	// MemoryOpAppend：追加一个新版本。
	MemoryOpAppend = "append"
	// MemoryOpSupersede：追加新版本并取代旧版本。
	MemoryOpSupersede = "supersede"
	// MemoryOpRetire：退役（闭合有效区间，不删内容）。
	MemoryOpRetire = "retire"
	// MemoryOpDreamDiff：记一条「dream 只提 diff」的提议（提议本身入账，应用另走
	// append/supersede）。
	MemoryOpDreamDiff = "dream_diff"
)

// MemoryListOptions 控制列哪些行。
type MemoryListOptions struct {
	// IncludeSuperseded 为真时把已闭合有效区间的历史版本也列出来。
	IncludeSuperseded bool
	// OnlyActive 为真时只列 state='active' 的行（两段式披露的读面口径：
	// 注入块/导出只给 active；治理面要隔离态条目用 ListMemoryEntriesByState）。
	OnlyActive bool
	// Limit <= 0 时用默认 200。
	Limit int
}

// memoryPayload 是校验并归一化后的写入载荷。
type memoryPayload struct {
	TopicKey         string
	Kind             string
	Title            string
	SummaryLine      string
	Tags             []string
	RuleRef          string
	SourceSessionIDs []int64
	WrittenBy        string
	State            string
	Body             string
	ContentDigest    string
}

// memoryQueryer 是 *sql.DB 与 *sql.Tx 共有的查询入口。
type memoryQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// memoryScanner 是 *sql.Row 与 *sql.Rows 共有的扫描入口。
type memoryScanner interface {
	Scan(dest ...any) error
}

const memoryEntryCols = `id, scope_type, scope_id, topic_key, version, kind, title, summary_line,
	array_to_json(tags)::text, rule_ref, array_to_json(source_session_ids)::text,
	written_by, content_digest, state, supersedes_id, valid_from, valid_to, superseded_by, created_at`

// scanMemoryEntry 把一行索引读成结构体。
func scanMemoryEntry(row memoryScanner) (*MemoryEntry, error) {
	var (
		e           MemoryEntry
		tagsJSON    []byte
		sourcesJSON []byte
	)
	if err := row.Scan(&e.ID, &e.ScopeType, &e.ScopeID, &e.TopicKey, &e.Version, &e.Kind,
		&e.Title, &e.SummaryLine, &tagsJSON, &e.RuleRef, &sourcesJSON, &e.WrittenBy,
		&e.ContentDigest, &e.State, &e.SupersedesID, &e.ValidFrom, &e.ValidTo, &e.SupersededBy,
		&e.CreatedAt); err != nil {
		return nil, err
	}
	e.Tags = []string{}
	if len(tagsJSON) > 0 {
		if err := json.Unmarshal(tagsJSON, &e.Tags); err != nil {
			return nil, fmt.Errorf("解析记忆标签失败: %w", err)
		}
	}
	e.SourceSessionIDs = []int64{}
	if len(sourcesJSON) > 0 {
		if err := json.Unmarshal(sourcesJSON, &e.SourceSessionIDs); err != nil {
			return nil, fmt.Errorf("解析记忆来源会话失败: %w", err)
		}
	}
	return &e, nil
}

// AppendMemoryEntry 追加一个新版本的记忆条目（**永不原地改写**）。
//
// 语义：同一个 scope 内同一个 topic_key 的每一次「内容变了」都表现为**追加**
// 一行 version+1 的新索引行；若该主题已有当前版本，新行 supersedes_id 指向它，
// 旧行被闭合有效区间（valid_to 非空、superseded_by 指向新行）。旧行一个字都不
// 改 —— 数据库触发器也不许改（schema_memory.sql）。
//
// 错误语义（全部是 typed error，调用方据此 fail-closed 地处理自己的链路）：
//   - scope 无效 ⇒ ErrMemoryScopeRequired（未 scope 的读写一律拒绝）。
//   - kind 不是 note/link ⇒ ErrMemoryKindInvalid。
//   - rule_ref 不是 agent_prompts:<id> 指针 ⇒ ErrMemoryRuleRefInvalid。
//   - 索引达到硬上限 ⇒ ErrMemoryIndexFull（响亮拒绝，绝不静默丢弃）。
//   - 并发写同一主题 ⇒ ErrMemoryVersionConflict。
//
// X4（方案 v10 §6 T2）：本入口是**基础写入**，state 默认 active（host 语义）。
// agent（模型）写路径走 AppendMemoryEntryChecked —— 默认落 quarantined；
// 治理/晋级用 host 通道 AppendMemoryEntryActive。
func (d *DB) AppendMemoryEntry(scope MemoryScope, in MemoryEntryInput) (*MemoryEntry, error) {
	entry, err := d.appendMemoryEntryWithDefault(scope, in, MemoryStateActive)
	if err != nil {
		// 基础写入没有前置余量检查（那是 Checked 的闸），上限错误在事务内以
		// 裸哨兵浮出——这里统一翻译成类型化 *MemoryCapError（「截断必须响亮」
		// + 「可区分」两条纪律对 host 通道同样成立）。
		return nil, d.MemoryCapErrorFor(scope, in, err)
	}
	return entry, nil
}

// AppendMemoryEntryActive 是 host 通道（治理/晋级/人工信任写入）：强制落
// state='active'，无视 in.State。agent 写路径**不许**用它 —— 模型写入即隔离
// 是 AppendMemoryEntryChecked 的职责。
func (d *DB) AppendMemoryEntryActive(scope MemoryScope, in MemoryEntryInput) (*MemoryEntry, error) {
	in.State = MemoryStateActive
	return d.appendMemoryEntryWithDefault(scope, in, MemoryStateActive)
}

// appendMemoryEntryWithDefault 是追加语义的唯一写入体：defaultState 决定
// in.State 为空时落什么态（agent Checked 路径隔离、host 路径 active ——
// 两条通道共用同一份 append-only / scope 强制 / 上限检查纪律）。
func (d *DB) appendMemoryEntryWithDefault(scope MemoryScope, in MemoryEntryInput, defaultState string) (*MemoryEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if !scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	payload, err := normalizeMemoryInput(in)
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启记忆写入事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := lockMemoryScope(tx, scope); err != nil {
		return nil, err
	}
	entry, err := appendMemoryVersion(tx, scope, payload, 0, defaultState)
	if err != nil {
		return nil, err
	}
	if err := enforceMemoryIndexCaps(tx, scope); err != nil {
		return nil, err
	}
	if err := appendMemoryLog(tx, scope, MemoryOpAppend, payload.WrittenBy, &entry.ID, map[string]any{
		"topic_key": payload.TopicKey,
		"version":   entry.Version,
		"kind":      payload.Kind,
		"state":     entry.State,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交记忆写入事务失败: %w", err)
	}
	return entry, nil
}

// SupersedeMemoryEntry 追加一个新版本，并显式取代给定的那一条旧版本。
//
// 与 AppendMemoryEntry 的区别：调用方**指名**要取代哪一行（dream diff 的「这条
// 取代 #123」形状）。被指名的行必须：属于同一 scope、是它主题的当前版本
// （valid_to 为空）。否则报 ErrMemoryScopeMismatch / ErrMemoryEntryNotFound。
//
// 取代同样不删任何东西：旧行只是被闭合有效区间（内容不变，仍可读）。
//
// X4（方案 v10 §6 T2）：取代也是**晋级/披露转态**的双时态通道 —— 晋级 =
// 追加 state='active' 的新版本并闭合隔离态旧行（host-only，见
// PromoteMemoryEntry）。取代的新版本 state 默认 active（host 语义；显式传入
// in.State 可覆盖）。
func (d *DB) SupersedeMemoryEntry(scope MemoryScope, oldID int64, in MemoryEntryInput) (*MemoryEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if !scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	if oldID <= 0 {
		return nil, ErrMemoryEntryNotFound
	}
	payload, err := normalizeMemoryInput(in)
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启记忆取代事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := lockMemoryScope(tx, scope); err != nil {
		return nil, err
	}
	entry, err := appendMemoryVersion(tx, scope, payload, oldID, MemoryStateActive)
	if err != nil {
		return nil, err
	}
	if err := enforceMemoryIndexCaps(tx, scope); err != nil {
		return nil, err
	}
	if err := appendMemoryLog(tx, scope, MemoryOpSupersede, payload.WrittenBy, &entry.ID, map[string]any{
		"topic_key":  payload.TopicKey,
		"version":    entry.Version,
		"kind":       payload.Kind,
		"state":      entry.State,
		"supersedes": oldID,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交记忆取代事务失败: %w", err)
	}
	return entry, nil
}

// RetireMemoryEntry 退役一个条目：闭合它的有效区间，**不删内容**。
//
// 退役与取代的区别：取代指向一个新版本，退役没有后继（这条记忆不再有效，但仍可
// 查历史）。同一行只能闭合一次（DDL 触发器也守着这条）。
func (d *DB) RetireMemoryEntry(scope MemoryScope, id int64, actor string) error {
	if err := d.ensureMemorySchema(); err != nil {
		return err
	}
	if !scope.Valid() {
		return ErrMemoryScopeRequired
	}
	if id <= 0 {
		return ErrMemoryEntryNotFound
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("开启记忆退役事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := lockMemoryScope(tx, scope); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE memory_index SET valid_to=now()
 WHERE id=$1 AND scope_type=$2 AND scope_id=$3 AND valid_to IS NULL`, id, scope.Type, scope.ID)
	if err != nil {
		return fmt.Errorf("退役记忆条目失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMemoryEntryNotFound
	}
	if err := appendMemoryLog(tx, scope, MemoryOpRetire, actor, &id, map[string]any{}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交记忆退役事务失败: %w", err)
	}
	return nil
}

// PromoteMemoryEntry 晋级一条隔离态记忆（X4，方案 v10 §6 T2：host-only 转态）。
//
// 走 Supersede 双时态通道：同一主题追加 state='active' 的新版本（内容逐字段
// 继承），旧行被闭合有效区间（quarantined 的历史仍可查）。**没有就地翻转** ——
// append-only 触发器把 state 钉在内容列守卫里，这是结构性的，不是纪律性。
//
// 幂等：目标已是 active ⇒ 原样返回（重复点击不产生新版本）。
// 已被取代/退役的行不能晋级（它已不在当前索引里）。
func (d *DB) PromoteMemoryEntry(id int64, actor string) (*MemoryEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrMemoryEntryNotFound
	}
	e, err := d.GetMemoryEntry(id)
	if err != nil {
		return nil, err
	}
	if e.ValidTo != nil {
		return nil, fmt.Errorf("记忆条目 %d 已被取代或退役，不在当前索引里，不能晋级", id)
	}
	if e.State == MemoryStateActive {
		return e, nil // 幂等成功：已是生效态
	}
	body, err := d.ReadMemoryTopic(id)
	if err != nil {
		return nil, err
	}
	in := MemoryEntryInput{
		TopicKey:         e.TopicKey,
		Kind:             e.Kind,
		Title:            e.Title,
		SummaryLine:      e.SummaryLine,
		Tags:             e.Tags,
		RuleRef:          e.RuleRef,
		SourceSessionIDs: e.SourceSessionIDs,
		WrittenBy:        "host:" + strings.TrimSpace(actor),
		Body:             body,
		State:            MemoryStateActive,
	}
	return d.SupersedeMemoryEntry(MemoryScope{Type: e.ScopeType, ID: e.ScopeID}, id, in)
}

// RetireMemoryEntryHost 是 host-only 的按 id 退役入口：治理面只拿得到 id，
// scope 由条目自己带出来，再走既有 RetireMemoryEntry 的双时态通道
// （闭合有效区间，不删内容）。已被取代/退役的行报可区分错误（不是 NotFound，
// 调用方能分清「条目在但已不在当前索引」）。
func (d *DB) RetireMemoryEntryHost(id int64, actor string) error {
	if err := d.ensureMemorySchema(); err != nil {
		return err
	}
	if id <= 0 {
		return ErrMemoryEntryNotFound
	}
	e, err := d.GetMemoryEntry(id)
	if err != nil {
		return err
	}
	if e.ValidTo != nil {
		return fmt.Errorf("记忆条目 %d 已被取代或退役（当前索引外），无需再退役", id)
	}
	return d.RetireMemoryEntry(MemoryScope{Type: e.ScopeType, ID: e.ScopeID}, id, actor)
}

// ListMemoryEntriesByState 是治理面的跨 scope 分页列表（X4：GET
// /api/memory/entries?state= 的取数）。state 为空 = 全部当前行（active +
// quarantined）；只认 active/quarantined 两个值，其他值报错（fail-closed，
// 不把拼错的过滤词当「全部」）。返回值带总数供分页。
func (d *DB) ListMemoryEntriesByState(state string, limit, offset int) ([]*MemoryEntry, int, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	where := ` WHERE valid_to IS NULL`
	args := []any{}
	if state != "" {
		if state != MemoryStateActive && state != MemoryStateQuarantined {
			return nil, 0, fmt.Errorf("state 只接受 active|quarantined 或留空，收到 %q", state)
		}
		where += ` AND state=$1`
		args = append(args, state)
	}
	var total int
	if err := d.QueryRow(`SELECT count(*) FROM memory_index`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计记忆条目失败: %w", err)
	}
	query := `SELECT ` + memoryEntryCols + ` FROM memory_index` + where +
		fmt.Sprintf(` ORDER BY id DESC LIMIT %d OFFSET %d`, limit, offset)
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("列记忆条目失败: %w", err)
	}
	defer rows.Close()
	out := make([]*MemoryEntry, 0, 16)
	for rows.Next() {
		e, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("列记忆条目失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("列记忆条目失败: %w", err)
	}
	return out, total, nil
}

// ListMemoryEntries 列 scope 下的索引行。
//
// 默认只列**当前**行（valid_to 为空）：被取代与退役的行从当前索引里消失，但仍可
// 用 IncludeSuperseded 查出来。scope 过滤是强制的（§10.4：scope 过滤强制）。
// OnlyActive（X4 两段式披露的读面口径）再滤掉隔离态 —— 注入块与纯文本导出走
// 这条；治理面要看隔离态条目用 ListMemoryEntriesByState。
func (d *DB) ListMemoryEntries(scope MemoryScope, opts MemoryListOptions) ([]*MemoryEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if !scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 200
	}
	query := `SELECT ` + memoryEntryCols + ` FROM memory_index
 WHERE scope_type=$1 AND scope_id=$2`
	if !opts.IncludeSuperseded {
		query += ` AND valid_to IS NULL`
	}
	if opts.OnlyActive {
		query += ` AND state='active'`
	}
	query += ` ORDER BY topic_key, version DESC LIMIT $3`
	rows, err := d.Query(query, scope.Type, scope.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("列记忆索引失败: %w", err)
	}
	defer rows.Close()
	out := make([]*MemoryEntry, 0, 16)
	for rows.Next() {
		e, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("列记忆索引失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列记忆索引失败: %w", err)
	}
	return out, nil
}

// GetMemoryEntry 读一行索引（任何版本）。
//
// 被取代的行仍然读得到 —— 内容没被改过，只是有效区间闭合了（§10.2）。
func (d *DB) GetMemoryEntry(id int64) (*MemoryEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrMemoryEntryNotFound
	}
	e, err := scanMemoryEntry(d.QueryRow(`SELECT `+memoryEntryCols+` FROM memory_index WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMemoryEntryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读记忆条目失败: %w", err)
	}
	return e, nil
}

// ReadMemoryTopic 读某一条目的正文（按需读，不常驻上下文）。
//
// 读的时候验一遍摘要：body 与 body_digest 对不上 ⇒ 内容被改过 ⇒ 报错
// （ErrMemoryTopicDigestMismatch），而不是把被改过的正文当成记忆喂给模型。
func (d *DB) ReadMemoryTopic(entryID int64) (string, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return "", err
	}
	if entryID <= 0 {
		return "", ErrMemoryEntryNotFound
	}
	var body, digest string
	err := d.QueryRow(`SELECT body, body_digest FROM memory_topic WHERE entry_id=$1`, entryID).
		Scan(&body, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrMemoryEntryNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读记忆正文失败: %w", err)
	}
	if digest != "" && digest != digestText(body) {
		return "", ErrMemoryTopicDigestMismatch
	}
	return body, nil
}

// ExportMemoryIndex 导出该 scope 当前索引的**纯文本**。
//
// 为什么是纯文本：记忆必须可查看、可编辑、可禁用（§11.8-2）。纯文本能被人直接
// 读、直接改，也能直接塞进 agent 的提示上下文（同一份东西两处用，不做二次格式
// 转换 ⇒ 看到的与注入的是同一份）。
//
// 导出的是当前索引里 **state='active'** 的行（X4 两段式披露）：被取代的条目以
// 新版本出现在同一个主题下，隔离态条目不进导出（要查全量用
// ListMemoryEntriesByState）。
func (d *DB) ExportMemoryIndex(scope MemoryScope) (string, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return "", err
	}
	if !scope.Valid() {
		return "", ErrMemoryScopeRequired
	}
	return exportMemoryIndexFiltered(d, scope, true)
}

// ListMemoryWriteLog 列该 scope 的写入账（只追加）。
//
// 账按 scope 串成哈希链 ⇒ 事后能验「这条索引是不是账上那条链上的」。
func (d *DB) ListMemoryWriteLog(scope MemoryScope, limit int) ([]*MemoryWriteLogEntry, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if !scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.Query(`SELECT id, scope_type, scope_id, op, actor, entry_id,
 prev_hash, new_hash, detail, created_at
 FROM memory_write_log WHERE scope_type=$1 AND scope_id=$2 ORDER BY id LIMIT $3`,
		scope.Type, scope.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("列记忆写入账失败: %w", err)
	}
	defer rows.Close()
	out := make([]*MemoryWriteLogEntry, 0, 16)
	for rows.Next() {
		var (
			rec    MemoryWriteLogEntry
			detail []byte
		)
		if err := rows.Scan(&rec.ID, &rec.ScopeType, &rec.ScopeID, &rec.Op, &rec.Actor,
			&rec.EntryID, &rec.PrevHash, &rec.NewHash, &detail, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("列记忆写入账失败: %w", err)
		}
		rec.Detail = json.RawMessage(detail)
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列记忆写入账失败: %w", err)
	}
	return out, nil
}

// RecordMemoryDreamDiff 记一条「dream 只提 diff」的提议（入账，不落索引）。
//
// 为什么单独记一条：dream 提的 diff 是**提议**，模型只提议、确定性层裁决
// （§8 红线第 2 条）。提议本身必须留痕（谁在什么时候提了什么），但它**不**改
// 索引 —— 要落索引得走 Append/Supersede。
func (d *DB) RecordMemoryDreamDiff(scope MemoryScope, actor string, diff json.RawMessage) (int64, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return 0, err
	}
	if !scope.Valid() {
		return 0, ErrMemoryScopeRequired
	}
	if len(diff) == 0 {
		diff = json.RawMessage(`{}`)
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启记忆提议事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := lockMemoryScope(tx, scope); err != nil {
		return 0, err
	}
	id, err := appendMemoryLogID(tx, scope, MemoryOpDreamDiff, actor, nil, json.RawMessage(diff))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交记忆提议事务失败: %w", err)
	}
	return id, nil
}

// ---------- 内部实现 ----------

// appendMemoryVersion 在事务里追加一个主题的新版本。
//
// explicitPrevID > 0 ⇒ 显式取代那一行（它必须属于同一 scope、是该主题的当前
// 版本）；= 0 ⇒ 自动取该主题的当前版本（没有就当新主题，version 从 1 起）。
// defaultState 是 in.State 为空时落的两段式披露态（agent Checked 路径隔离、
// host 路径 active —— 由各写入入口决定，见 appendMemoryEntryWithDefault）。
//
// 事务里做三件事：追加新行、闭合旧行、写正文。任何一步失败 ⇒ 整体回滚 ⇒
// 「旧行被改了」这种半截状态不会出现。
func appendMemoryVersion(tx *sql.Tx, scope MemoryScope, p memoryPayload, explicitPrevID int64, defaultState string) (*MemoryEntry, error) {
	// state 的取值只有一个定义处：显式传入的 p.State 优先，否则落入口默认。
	state := p.State
	if state == "" {
		state = defaultState
	}
	if state != MemoryStateActive && state != MemoryStateQuarantined {
		return nil, fmt.Errorf("记忆披露态只接受 active|quarantined，收到 %q", state)
	}
	var (
		prevID      int64
		prevVersion int
		topicKey    = p.TopicKey
	)
	if explicitPrevID > 0 {
		var (
			oldScopeType string
			oldScopeID   int64
			oldTopicKey  string
			oldVersion   int
			oldValidTo   *time.Time
		)
		err := tx.QueryRow(`SELECT scope_type, scope_id, topic_key, version, valid_to
 FROM memory_index WHERE id=$1 FOR UPDATE`, explicitPrevID).
			Scan(&oldScopeType, &oldScopeID, &oldTopicKey, &oldVersion, &oldValidTo)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMemoryEntryNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("读被取代的记忆条目失败: %w", err)
		}
		if oldScopeType != scope.Type || oldScopeID != scope.ID {
			return nil, ErrMemoryScopeMismatch
		}
		if oldValidTo != nil {
			return nil, fmt.Errorf("记忆条目 %d 已被取代或退役，不能再次取代", explicitPrevID)
		}
		if p.TopicKey != "" && p.TopicKey != oldTopicKey {
			return nil, fmt.Errorf("取代必须沿用原主题键（%s），收到 %q", oldTopicKey, p.TopicKey)
		}
		prevID, prevVersion = explicitPrevID, oldVersion
		// 取代必须沿用原主题键：它就是同一主题的新版本。
		topicKey = oldTopicKey
	} else {
		if p.TopicKey == "" {
			return nil, errors.New("记忆主题键不能为空")
		}
		err := tx.QueryRow(`SELECT id, version FROM memory_index
 WHERE scope_type=$1 AND scope_id=$2 AND topic_key=$3 AND valid_to IS NULL
 ORDER BY version DESC LIMIT 1 FOR UPDATE`, scope.Type, scope.ID, topicKey).Scan(&prevID, &prevVersion)
		if errors.Is(err, sql.ErrNoRows) {
			prevID, prevVersion = 0, 0
		} else if err != nil {
			return nil, fmt.Errorf("读记忆当前版本失败: %w", err)
		}
	}
	version := prevVersion + 1

	tagsJSON, err := json.Marshal(p.Tags)
	if err != nil {
		return nil, fmt.Errorf("序列化记忆标签失败: %w", err)
	}
	sourcesJSON, err := json.Marshal(p.SourceSessionIDs)
	if err != nil {
		return nil, fmt.Errorf("序列化记忆来源会话失败: %w", err)
	}

	var supersedes any
	if prevID > 0 {
		supersedes = prevID
	}
	entry, err := scanMemoryEntry(tx.QueryRow(`INSERT INTO memory_index(
 scope_type, scope_id, topic_key, version, kind, title, summary_line, tags, rule_ref,
 source_session_ids, written_by, content_digest, state, supersedes_id)
VALUES($1,$2,$3,$4,$5,$6,$7,
 ARRAY(SELECT jsonb_array_elements_text(($8::text)::jsonb)),
 $9,
 ARRAY(SELECT jsonb_array_elements_text(($10::text)::jsonb))::bigint[],
 $11,$12,$13,$14)
RETURNING `+memoryEntryCols,
		scope.Type, scope.ID, topicKey, version, p.Kind, p.Title, p.SummaryLine,
		string(tagsJSON), p.RuleRef, string(sourcesJSON), p.WrittenBy, p.ContentDigest,
		state, supersedes))
	if err != nil {
		if memoryUniqueViolation(err) {
			return nil, ErrMemoryVersionConflict
		}
		return nil, fmt.Errorf("写记忆索引失败: %w", err)
	}

	if prevID > 0 {
		// 闭合旧行的有效区间（唯一被允许的 UPDATE：valid_to + superseded_by）。
		res, err := tx.Exec(`UPDATE memory_index SET valid_to=now(), superseded_by=$2
 WHERE id=$1 AND valid_to IS NULL`, prevID, entry.ID)
		if err != nil {
			return nil, fmt.Errorf("闭合记忆有效区间失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrMemoryEntryNotFound
		}
	}

	if _, err := tx.Exec(`INSERT INTO memory_topic(entry_id, body, body_digest)
 VALUES($1,$2,$3)`, entry.ID, p.Body, p.ContentDigest); err != nil {
		return nil, fmt.Errorf("写记忆正文失败: %w", err)
	}
	return entry, nil
}

// enforceMemoryIndexCaps 检查索引的硬上限：条目数与导出字节数。
//
// 超限 ⇒ 报错 ⇒ 上层事务回滚 ⇒ **写入被拒**（响亮），不是把多出来的条目悄悄
// 丢掉（§11.8-4：截断必须响亮；§10.5-2：写入时拒绝，绝不静默截断）。
func enforceMemoryIndexCaps(tx *sql.Tx, scope MemoryScope) error {
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM memory_index
 WHERE scope_type=$1 AND scope_id=$2 AND valid_to IS NULL`, scope.Type, scope.ID).Scan(&count); err != nil {
		return fmt.Errorf("统计记忆索引条目失败: %w", err)
	}
	if count > MemoryIndexMaxEntries {
		return ErrMemoryIndexFull
	}
	text, err := exportMemoryIndexFiltered(tx, scope, false)
	if err != nil {
		return err
	}
	if len(text) > MemoryIndexMaxBytes {
		return ErrMemoryIndexFull
	}
	return nil
}

// exportMemoryIndexFiltered 渲染该 scope 当前索引的纯文本。
//
// onlyActive 为真 ⇒ 只渲染 state='active' 的行（agent 读面：两段式披露只给
// active）；为假 ⇒ 全部当前行（上限检查的口径：隔离态条目也占 scope 预算，
// 否则一个写满隔离态的 scope 永远撞不到字节上限）。
func exportMemoryIndexFiltered(q memoryQueryer, scope MemoryScope, onlyActive bool) (string, error) {
	query := `SELECT ` + memoryEntryCols + ` FROM memory_index
 WHERE scope_type=$1 AND scope_id=$2 AND valid_to IS NULL`
	if onlyActive {
		query += ` AND state='active'`
	}
	query += `
 ORDER BY topic_key, version`
	rows, err := q.Query(query, scope.Type, scope.ID)
	if err != nil {
		return "", fmt.Errorf("导出记忆索引失败: %w", err)
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("# 记忆索引 · scope=" + scope.String() + "\n")
	b.WriteString("# 规则不住在索引里：rule_ref 只是 agent_prompts 的指针。\n")
	b.WriteString("# 本索引是 append-only 的版本化索引：被取代的条目以新版本追加，旧版本只闭合有效区间。\n")
	if onlyActive {
		b.WriteString("# 本导出只含 state=active 的条目（两段式披露）：隔离态条目经 host 晋级后才可见。\n")
	}
	for rows.Next() {
		e, err := scanMemoryEntry(rows)
		if err != nil {
			return "", fmt.Errorf("导出记忆索引失败: %w", err)
		}
		writeMemoryEntryText(&b, e)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("导出记忆索引失败: %w", err)
	}
	return b.String(), nil
}

// writeMemoryEntryText 把一条索引行写进纯文本导出。
func writeMemoryEntryText(b *strings.Builder, e *MemoryEntry) {
	b.WriteString("\n## " + e.TopicKey + " · v" + strconv.Itoa(e.Version) + " · " + e.Kind + "\n")
	b.WriteString("- 条目: #" + strconv.FormatInt(e.ID, 10) + "\n")
	b.WriteString("- 标题: " + e.Title + "\n")
	b.WriteString("- 摘要: " + e.SummaryLine + "\n")
	if len(e.Tags) > 0 {
		b.WriteString("- 标签: " + strings.Join(e.Tags, ", ") + "\n")
	}
	if e.RuleRef != "" {
		b.WriteString("- 规则指针: " + e.RuleRef + "\n")
	}
	if len(e.SourceSessionIDs) > 0 {
		parts := make([]string, 0, len(e.SourceSessionIDs))
		for _, id := range e.SourceSessionIDs {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
		b.WriteString("- 来源会话: " + strings.Join(parts, ", ") + "\n")
	}
	b.WriteString("- 写入者: " + e.WrittenBy + "\n")
	b.WriteString("- 内容摘要: " + e.ContentDigest + "\n")
}

// normalizeMemoryInput 校验并归一化写入载荷。
//
// 校验项与理由：
//   - kind ∈ {note, link}：**没有 rule** ⇒ 索引结构上装不下可执行规则。
//   - rule_ref 形如 agent_prompts:<id>：指针装不下规则正文。
//   - 标题/摘要长度上限：索引是常驻上下文。
//   - BodyDigest 与实算摘要一致：调用方算的必须是同一份内容。
//
// 主题键的空值不在这里判：取代时主题键由被取代的那一行继承，只有「新建主题」
// 才要求非空 ⇒ 判在 appendMemoryVersion 里（两条路径要求不同）。
func normalizeMemoryInput(in MemoryEntryInput) (memoryPayload, error) {
	p := memoryPayload{
		TopicKey:         strings.TrimSpace(in.TopicKey),
		Kind:             strings.TrimSpace(in.Kind),
		Title:            strings.TrimSpace(in.Title),
		SummaryLine:      strings.TrimSpace(in.SummaryLine),
		RuleRef:          strings.TrimSpace(in.RuleRef),
		WrittenBy:        strings.TrimSpace(in.WrittenBy),
		State:            strings.TrimSpace(in.State),
		Body:             in.Body,
		Tags:             []string{},
		SourceSessionIDs: []int64{},
	}
	if p.State != "" && p.State != MemoryStateActive && p.State != MemoryStateQuarantined {
		return p, fmt.Errorf("记忆披露态只接受 active|quarantined，收到 %q", p.State)
	}
	if p.Kind == "" {
		p.Kind = MemoryKindNote
	}
	if p.Kind != MemoryKindNote && p.Kind != MemoryKindLink {
		return p, ErrMemoryKindInvalid
	}
	if len([]rune(p.Title)) > MemoryTitleMaxLen {
		return p, fmt.Errorf("记忆标题超过 %d 字", MemoryTitleMaxLen)
	}
	if len([]rune(p.SummaryLine)) > MemorySummaryMaxLen {
		return p, fmt.Errorf("记忆摘要超过 %d 字", MemorySummaryMaxLen)
	}
	if p.RuleRef != "" && !memoryRuleRefPattern.MatchString(p.RuleRef) {
		return p, ErrMemoryRuleRefInvalid
	}
	for _, t := range in.Tags {
		t = strings.TrimSpace(t)
		if t != "" {
			p.Tags = append(p.Tags, t)
		}
	}
	p.SourceSessionIDs = append(p.SourceSessionIDs, in.SourceSessionIDs...)
	p.ContentDigest = digestText(p.Body)
	if in.BodyDigest != "" && in.BodyDigest != p.ContentDigest {
		return p, fmt.Errorf("记忆正文摘要不匹配：传入 %s，实算 %s", in.BodyDigest, p.ContentDigest)
	}
	return p, nil
}

// appendMemoryLog 往写入账里追加一行（哈希链）。
func appendMemoryLog(tx *sql.Tx, scope MemoryScope, op, actor string, entryID *int64, detail map[string]any) error {
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("序列化记忆写入账明细失败: %w", err)
	}
	_, err = appendMemoryLogID(tx, scope, op, actor, entryID, detailJSON)
	return err
}

// appendMemoryLogID 往写入账里追加一行，返回它的 id。
//
// 哈希链按 scope 串：prev_hash = 该 scope 上一条账的 new_hash；new_hash =
// sha256(prev_hash ‖ op ‖ actor ‖ entry_id ‖ detail)。链断了就能验出来。
func appendMemoryLogID(tx *sql.Tx, scope MemoryScope, op, actor string, entryID *int64, detail json.RawMessage) (int64, error) {
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var prev string
	err := tx.QueryRow(`SELECT new_hash FROM memory_write_log
 WHERE scope_type=$1 AND scope_id=$2 ORDER BY id DESC LIMIT 1`, scope.Type, scope.ID).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		prev = ""
	} else if err != nil {
		return 0, fmt.Errorf("读记忆写入账链头失败: %w", err)
	}
	entryKey := ""
	if entryID != nil {
		entryKey = strconv.FormatInt(*entryID, 10)
	}
	newHash := memoryChainHash(prev, op, actor, entryKey, string(detail))
	var id int64
	err = tx.QueryRow(`INSERT INTO memory_write_log(
 scope_type, scope_id, op, actor, entry_id, prev_hash, new_hash, detail)
VALUES($1,$2,$3,$4,$5,$6,$7,($8::text)::jsonb) RETURNING id`,
		scope.Type, scope.ID, op, actor, entryID, prev, newHash, string(detail)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写记忆写入账失败: %w", err)
	}
	return id, nil
}

// lockMemoryScope 拿 scope 级的**事务** advisory 锁。
//
// 为什么需要：写入账是按 scope 串起来的哈希链 ⇒ 两个并发写者若读到同一个
// prev_hash，链就分叉了。事务级锁把同一 scope 的写入串行化（不同 scope 互不
// 阻塞）。
func lockMemoryScope(tx *sql.Tx, scope MemoryScope) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, memoryScopeLockKey(scope)); err != nil {
		return fmt.Errorf("获取记忆 scope 锁失败: %w", err)
	}
	return nil
}

// memoryScopeLockKey 把 scope 映射成 advisory 锁键（正数，取 sha256 前 8 字节）。
func memoryScopeLockKey(scope MemoryScope) int64 {
	sum := sha256.Sum256([]byte(scope.String()))
	return int64(binary.BigEndian.Uint64(sum[:8]) >> 1)
}

// memoryChainHash 算写入账的链式摘要。
func memoryChainHash(prev, op, actor, entryKey, detail string) string {
	h := sha256.New()
	for _, part := range []string{prev, op, actor, entryKey, detail} {
		h.Write([]byte(part))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// digestText 是内容的 sha256 十六进制摘要（正文一旦写下就不会变 ⇒ 摘要用来验
// 「没被改过」）。
func digestText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// memoryUniqueViolation 判断是不是唯一约束冲突（23505）—— 记忆索引的版本唯一
// 约束就是并发写的 CAS 点：撞上它 ⇒ 报可重试的版本冲突，而不是把冲突吞掉。
func memoryUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
