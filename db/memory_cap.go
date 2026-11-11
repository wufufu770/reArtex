// 记忆索引的**硬上限**：写时拒绝、绝不静默截断；失败是**可区分**的类型化错误。
//
// 依据（session-groupchat-proposal.md §10.5 第 2 条 / §11.8 第 4 条）：
//
//	「索引 + 明细，索引有硬上限，写入时拒绝，绝不静默截断」
//	「索引硬上限 + 截断必须响亮（超限时拒绝写入并提示，绝不静默丢弃）」
//
// 两半职责：
//
//	① **拒绝** —— db/memory.go 的 enforceMemoryIndexCaps 在写事务里守着上限：
//	   写多了整个事务回滚 ⇒ 写入被拒，**没有任何条目被悄悄丢掉**。那一半已经在，
//	   本文件不重复实现写路径。
//	② **可区分** —— ErrMemoryIndexFull 是一个扁平哨兵：调用方只知道「写不进去」，
//	   不知道是**条目数**上限（MemoryIndexMaxEntries）还是**字节**上限
//	   （MemoryIndexMaxBytes）⇒ 人没法判断该退役哪几条、还差几。
//	   本文件给出带上下文的类型化错误（哪条上限、限值多少、当前多少、还差几）。
//
// ⚠️ 阈值**只**来自 db/memory.go 的代码常量（§8 红线第 3 条：新增安全边界进代码
//    常量，不进可热改的 DB/settings）。本文件不另立一份数字。
//
// ⚠️ 记忆是提示，不是安全边界：上限拒绝**只**意味着「这条记忆写不进去」，
//    不影响任何放行/拒绝判定。

package db

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 上限的种类（哪条上限被撞到了）。
const (
	// MemoryCapKindEntries 是条目数上限。
	MemoryCapKindEntries = "entries"
	// MemoryCapKindBytes 是纯文本导出的字节上限。
	MemoryCapKindBytes = "bytes"
)

// MemoryCapByteMargin 是字节上限判定预留的余量（字节）。
//
// 为什么需要余量：纯文本导出里有「- 条目: #<id>」这一行，而新条目的 id 是写库
// 时才分配的 ⇒ 写之前算不出确切长度。余量 32 字节覆盖了 bigint 十进制位数的
// 最大差（19 位 vs 1 位 = 18）再加一点余量 ⇒ 判定偏保守（宁可早拒，不可放过）。
//
// ⚠️ 精确判定仍在 db/memory.go 的事务内检查里（它量的是写之后的真实导出长度），
//
//	本文件的余量只用于**写前预判**，不替代它。
const MemoryCapByteMargin = 32

// MemoryCapError 是「撞上索引上限」的类型化错误。
//
// 它 unwrap 到 ErrMemoryIndexFull ⇒ 既有调用方用 errors.Is(err, ErrMemoryIndexFull)
// 判定的地方**不用改**；新调用方用 errors.As 拿具体细节。
type MemoryCapError struct {
	// Scope 是被拒写入的归属范围。
	Scope MemoryScope
	// Kind 是撞到的那条上限（MemoryCapKindEntries / MemoryCapKindBytes）。
	Kind string
	// Limit 是该上限的限值。
	Limit int64
	// Observed 是实测值（口径随 Kind 而变：条目数 or 字节数）。
	Observed int64
	// Remaining 是还能再写多少（口径同 Observed）。
	Remaining int64
}

// Error 实现 error。
func (e *MemoryCapError) Error() string {
	if e == nil {
		return "记忆索引已达硬上限"
	}
	unit := "条"
	if e.Kind == MemoryCapKindBytes {
		unit = "字节"
	}
	return fmt.Sprintf("记忆索引已达硬上限（%s）：上限 %d %s，实测 %d %s，还能再写 %d %s",
		e.Kind, e.Limit, unit, e.Observed, unit, e.Remaining, unit)
}

// Unwrap 让 errors.Is(err, ErrMemoryIndexFull) 继续成立。
func (e *MemoryCapError) Unwrap() error { return ErrMemoryIndexFull }

// MemoryIndexHeadroom 是一个 scope 的索引余量。
type MemoryIndexHeadroom struct {
	Scope MemoryScope `json:"scope"`
	// Entries 是当前有效条目数（valid_to IS NULL）。
	Entries int `json:"entries"`
	// Bytes 是当前索引纯文本导出的字节数（与 db/memory.go 的口径一致）。
	Bytes int `json:"bytes"`
	// MaxEntries / MaxBytes 是代码常量里的限值。
	MaxEntries int `json:"max_entries"`
	MaxBytes   int `json:"max_bytes"`
	// EntriesRemaining / BytesRemaining 是还能再写多少。
	EntriesRemaining int `json:"entries_remaining"`
	BytesRemaining   int `json:"bytes_remaining"`
	// NearestCap 是先撞到的那条上限（entries / bytes）。
	NearestCap string `json:"nearest_cap"`
	// EntriesFull / BytesFull 是两条上限各自的「已经满了」标志。
	EntriesFull bool `json:"entries_full"`
	BytesFull   bool `json:"bytes_full"`
}

// MemoryIndexHeadroom 读一个 scope 的索引余量。
//
// 口径与 db/memory.go 的 enforceMemoryIndexCaps **逐字一致**（同一条导出 SQL、
// 同一条计数 SQL）⇒ 这里算出的「满了」与写路径拒绝的那一刻一致。
func (d *DB) MemoryIndexHeadroom(scope MemoryScope) (*MemoryIndexHeadroom, error) {
	if err := d.ensureMemorySchema(); err != nil {
		return nil, err
	}
	if !scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	var entries int
	if err := d.QueryRow(`SELECT count(*) FROM memory_index
 WHERE scope_type=$1 AND scope_id=$2 AND valid_to IS NULL`, scope.Type, scope.ID).Scan(&entries); err != nil {
		return nil, fmt.Errorf("统计记忆索引条目失败: %w", err)
	}
	text, err := exportMemoryIndexFiltered(d, scope, false)
	if err != nil {
		return nil, err
	}
	h := &MemoryIndexHeadroom{
		Scope:      scope,
		Entries:    entries,
		Bytes:      len(text),
		MaxEntries: MemoryIndexMaxEntries,
		MaxBytes:   MemoryIndexMaxBytes,
	}
	h.EntriesRemaining = MemoryIndexMaxEntries - entries
	if h.EntriesRemaining < 0 {
		h.EntriesRemaining = 0
	}
	h.BytesRemaining = MemoryIndexMaxBytes - len(text)
	if h.BytesRemaining < 0 {
		h.BytesRemaining = 0
	}
	h.EntriesFull = entries >= MemoryIndexMaxEntries
	h.BytesFull = len(text) >= MemoryIndexMaxBytes
	// 先撞到哪条：看**相对**余量（比例），不是绝对值 —— 两种上限量纲不同。
	entriesRatio := float64(h.EntriesRemaining) / float64(MemoryIndexMaxEntries)
	bytesRatio := float64(h.BytesRemaining) / float64(MemoryIndexMaxBytes)
	if entriesRatio <= bytesRatio {
		h.NearestCap = MemoryCapKindEntries
	} else {
		h.NearestCap = MemoryCapKindBytes
	}
	return h, nil
}

// CheckMemoryIndexCap 预判「现在写会不会撞上上限」。
//
// 返回 nil ⇒ 大概率写得进去（但**不保证**：并发写可能在这之后把额度占掉 ⇒
// 那时由 db/memory.go 的事务内检查兜底，写入仍然被拒，绝不静默截断）。
//
// 返回 *MemoryCapError ⇒ 明确写不进去了，错误里带着「哪条上限 / 限值 / 还差几」。
func (d *DB) CheckMemoryIndexCap(scope MemoryScope) error {
	h, err := d.MemoryIndexHeadroom(scope)
	if err != nil {
		return err
	}
	if h.EntriesFull {
		return &MemoryCapError{
			Scope:     scope,
			Kind:      MemoryCapKindEntries,
			Limit:     int64(MemoryIndexMaxEntries),
			Observed:  int64(h.Entries),
			Remaining: 0,
		}
	}
	if h.BytesFull {
		return &MemoryCapError{
			Scope:     scope,
			Kind:      MemoryCapKindBytes,
			Limit:     int64(MemoryIndexMaxBytes),
			Observed:  int64(h.Bytes),
			Remaining: 0,
		}
	}
	return nil
}

// AppendMemoryEntryChecked 是「带上限语义」的写入入口 —— 也是 **agent（模型）
// 写路径**（X4，方案 v10 §6 T2）：写入默认落 state='quarantined'（模型写入即
// 隔离）。治理/晋级要写 active 走 host 通道 AppendMemoryEntryActive。
//
// 语义：
//   - 写前预判（CheckMemoryIndexCap）：已经满了 ⇒ 直接返回类型化错误，**不动库**。
//   - 写（复用 db/memory.go 的唯一写入体 appendMemoryEntryWithDefault ⇒
//     append-only / scope 强制 / 事务内上限检查全部照旧；默认隔离态）。
//   - 写后若撞上上限（ErrMemoryIndexFull）⇒ 翻译成类型化错误：重读余量，按
//     「条目数是否已到上限」精确判定是哪条上限（写事务已回滚 ⇒ 重读到的就是
//     写之前的真实状态）。
//
// 隔离态条目不进注入块/检索面（两段式披露），要让它可见必须走 host-only 的
// PromoteMemoryEntry（Supersede 通道追加 active 新版本）—— 模型自己做不到。
func (d *DB) AppendMemoryEntryChecked(scope MemoryScope, in MemoryEntryInput) (*MemoryEntry, error) {
	if err := d.CheckMemoryIndexCap(scope); err != nil {
		return nil, err
	}
	entry, err := d.appendMemoryEntryWithDefault(scope, in, MemoryStateQuarantined)
	if err != nil {
		return nil, d.MemoryCapErrorFor(scope, in, err)
	}
	return entry, nil
}

// MemoryCapErrorFor 把「写被上限拒绝」翻译成类型化错误。
//
// 判定哪条上限（精确规则）：写事务已回滚 ⇒ 重读到的条目数是**写之前**的数。
// 写路径的检查顺序是「先条目数、后字节数」，所以：
//
//	条目数 ≥ 条目上限 ⇒ 撞的是条目上限；
//	否则                ⇒ 撞的是字节上限（条目数那条当时是过的）。
//
// 非上限错误原样返回（调用方不该把「scope 非法」当成上限问题）。
func (d *DB) MemoryCapErrorFor(scope MemoryScope, in MemoryEntryInput, err error) error {
	if err == nil || !errors.Is(err, ErrMemoryIndexFull) {
		return err
	}
	headroom, herr := d.MemoryIndexHeadroom(scope)
	if herr != nil {
		// 读不到余量 ⇒ 只报哨兵错误，不猜数字（fail-closed：宁可信息少，
		// 不可报错数字）。
		return err
	}
	if headroom.Entries >= MemoryIndexMaxEntries {
		return &MemoryCapError{
			Scope:     scope,
			Kind:      MemoryCapKindEntries,
			Limit:     int64(MemoryIndexMaxEntries),
			Observed:  int64(headroom.Entries),
			Remaining: 0,
		}
	}
	observed := int64(headroom.Bytes) + memoryEntryRenderedLen(in)
	return &MemoryCapError{
		Scope:     scope,
		Kind:      MemoryCapKindBytes,
		Limit:     int64(MemoryIndexMaxBytes),
		Observed:  observed,
		Remaining: int64(MemoryIndexMaxBytes - headroom.Bytes),
	}
}

// memoryEntryRenderedLen 估算一条索引行在纯文本导出里占多少字节。
//
// 用的是与 db/memory.go 的导出**同一个渲染器**（writeMemoryEntryText）⇒ 口径
// 不会漂移。id 与 version 用占位值（写入时才确定）⇒ 差值由 MemoryCapByteMargin
// 兜住。
func memoryEntryRenderedLen(in MemoryEntryInput) int64 {
	entry := &MemoryEntry{
		TopicKey:         strings.TrimSpace(in.TopicKey),
		Version:          1,
		Kind:             strings.TrimSpace(in.Kind),
		Title:            strings.TrimSpace(in.Title),
		SummaryLine:      strings.TrimSpace(in.SummaryLine),
		Tags:             in.Tags,
		RuleRef:          strings.TrimSpace(in.RuleRef),
		SourceSessionIDs: in.SourceSessionIDs,
		WrittenBy:        strings.TrimSpace(in.WrittenBy),
	}
	if entry.Kind == "" {
		entry.Kind = MemoryKindNote
	}
	var b strings.Builder
	writeMemoryEntryText(&b, entry)
	return int64(b.Len())
}

// FormatMemoryCapHint 给人一句「该做什么」的提示。
//
// 为什么单独一个函数：错误文案是给人看的，但**不能**由模型生成（§8 红线第 2 条）
// ⇒ 它必须是代码里写死的一句话。
func FormatMemoryCapHint(err error) string {
	var capErr *MemoryCapError
	if !errors.As(err, &capErr) {
		return ""
	}
	switch capErr.Kind {
	case MemoryCapKindEntries:
		return "记忆索引条目数已达上限（" + strconv.Itoa(MemoryIndexMaxEntries) +
			" 条）：请先退役或压缩旧条目，再写入。"
	case MemoryCapKindBytes:
		return "记忆索引字节数已达上限（" + strconv.Itoa(MemoryIndexMaxBytes) +
			" 字节）：请先退役或压缩旧条目，再写入。"
	default:
		return "记忆索引已达硬上限：请先退役或压缩旧条目，再写入。"
	}
}
