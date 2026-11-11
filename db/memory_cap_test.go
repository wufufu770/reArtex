package db

import (
	"errors"
	"strings"
	"testing"
)

// 记忆索引硬上限的验收用例。
//
// 钉住的是三条承诺：
//   1. **写时拒绝**：撞上限时写入失败，**没有任何条目被静默丢掉**；
//   2. **可区分**：失败是 *MemoryCapError，能分清是条目数上限还是字节上限，
//      且 errors.Is(err, ErrMemoryIndexFull) 仍然成立（既有调用方不用改）；
//   3. **不猜数字**：报出来的上限值来自代码常量，余量来自真实读数。

func TestMemoryIndexHeadroomFreshScope(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	h, err := d.MemoryIndexHeadroom(scope)
	if err != nil {
		t.Fatalf("读余量失败: %v", err)
	}
	if h.Entries != 0 {
		t.Errorf("新 scope 的条目数 = %d，期望 0", h.Entries)
	}
	if h.MaxEntries != MemoryIndexMaxEntries || h.MaxBytes != MemoryIndexMaxBytes {
		t.Errorf("上限值与代码常量不一致：%d / %d", h.MaxEntries, h.MaxBytes)
	}
	if h.EntriesRemaining != MemoryIndexMaxEntries {
		t.Errorf("条目余量 = %d，期望 %d", h.EntriesRemaining, MemoryIndexMaxEntries)
	}
	if h.EntriesFull || h.BytesFull {
		t.Error("新 scope 不该是满的")
	}
	if h.NearestCap != MemoryCapKindEntries && h.NearestCap != MemoryCapKindBytes {
		t.Errorf("最近的上限 = %q", h.NearestCap)
	}
}

func TestMemoryIndexCapRejectsWriteAndKeepsEverything(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	// 一路写到被拒为止。
	// X4（方案 v10 §6 T2）：Checked 是 agent 写路径（默认隔离态）——上限口径
	// 量的是**全部当前行**（含隔离态），但本用例断言导出可见性，所以用 host
	// 语义的基础写入（active），上限检查本身不变。
	var lastErr error
	written := 0
	for i := 0; i < MemoryIndexMaxEntries+20; i++ {
		_, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
			TopicKey:    "cap-" + strconvItoa(i),
			Title:       "上限压力条目 " + strconvItoa(i),
			SummaryLine: "用于验证索引上限写时拒绝",
			WrittenBy:   "tester",
		})
		if err != nil {
			lastErr = err
			break
		}
		written++
	}
	if lastErr == nil {
		t.Fatalf("写了 %d 条都没撞上限 —— 上限没生效？", written)
	}

	// ① 可区分：类型化错误。
	var capErr *MemoryCapError
	if !errors.As(lastErr, &capErr) {
		t.Fatalf("失败错误不是 *MemoryCapError：%v", lastErr)
	}
	if capErr.Kind != MemoryCapKindEntries && capErr.Kind != MemoryCapKindBytes {
		t.Errorf("上限种类 = %q，期望 entries 或 bytes", capErr.Kind)
	}
	if capErr.Limit <= 0 {
		t.Errorf("上限值 = %d，期望正数", capErr.Limit)
	}
	// ② 向后兼容：哨兵错误仍然可判定。
	if !errors.Is(lastErr, ErrMemoryIndexFull) {
		t.Errorf("类型化错误必须 unwrap 到 ErrMemoryIndexFull：%v", lastErr)
	}

	// ③ 绝不静默截断：被拒之后，索引里一条都没少，导出长度也没缩水。
	h, err := d.MemoryIndexHeadroom(scope)
	if err != nil {
		t.Fatalf("读余量失败: %v", err)
	}
	if h.Entries != written {
		t.Errorf("被拒后条目数 = %d，期望 %d（不许悄悄丢条目）", h.Entries, written)
	}
	text, err := d.ExportMemoryIndex(scope)
	if err != nil {
		t.Fatalf("导出索引失败: %v", err)
	}
	if !strings.Contains(text, "cap-0") {
		t.Error("最早的条目不见了 —— 索引被截断了")
	}
	if !strings.Contains(text, "cap-"+strconvItoa(written-1)) {
		t.Error("最后一条写入的条目不见了 —— 索引被截断了")
	}

	// ④ 契约：被拒的是「**这次写入会让索引超限**」⇒ 索引本身**不得**超限
	//    （既不静默截断，也不写超），而错误里的 Observed 是**投影后**的大小
	//    （当前 + 本次写入），必须大于上限。
	if capErr.Observed <= capErr.Limit {
		t.Errorf("错误的 Observed=%d 应当是投影后的大小，且必须大于上限 %d", capErr.Observed, capErr.Limit)
	}
	if h.Bytes > MemoryIndexMaxBytes {
		t.Errorf("索引字节数 %d 超了上限 %d —— 写超了", h.Bytes, MemoryIndexMaxBytes)
	}
	if h.Entries > MemoryIndexMaxEntries {
		t.Errorf("索引条目数 %d 超了上限 %d —— 写超了", h.Entries, MemoryIndexMaxEntries)
	}
}

func TestMemoryCapErrorMessageAndHint(t *testing.T) {
	capErr := &MemoryCapError{
		Scope:     MemoryScope{Type: MemoryScopeTaskHall, ID: 42},
		Kind:      MemoryCapKindEntries,
		Limit:     150,
		Observed:  150,
		Remaining: 0,
	}
	msg := capErr.Error()
	if !strings.Contains(msg, "entries") {
		t.Errorf("错误文案应带上限种类：%q", msg)
	}
	hint := FormatMemoryCapHint(capErr)
	if hint == "" {
		t.Error("必须给出人话提示")
	}
	if !strings.Contains(hint, "退役") {
		t.Errorf("提示应说清该做什么：%q", hint)
	}
	if FormatMemoryCapHint(errors.New("别的错")) != "" {
		t.Error("非上限错误不应给提示")
	}
}

func TestMemoryCapCheckRejectsFullScope(t *testing.T) {
	d := memoryRetrievalOpen(t)
	scope := memoryTestScope(t)

	// 未满 ⇒ 预判通过。
	if err := d.CheckMemoryIndexCap(scope); err != nil {
		t.Fatalf("未满的 scope 不该被拒：%v", err)
	}

	// 写满到被拒之后 ⇒ 预判给出类型化错误（同前：host 语义基础写入，
	// 保证导出/预判口径与条目可见性一致）。
	for i := 0; i < MemoryIndexMaxEntries+20; i++ {
		if _, err := d.AppendMemoryEntry(scope, MemoryEntryInput{
			TopicKey: "pre-" + strconvItoa(i),
			Title:    "预判条目",
		}); err != nil {
			break
		}
	}
	// 预判与写路径的契约必须自洽：
	//   - 索引**已到**上限 ⇒ 预判必须报满（且 unwrap 到 ErrMemoryIndexFull）；
	//   - 索引**还有余量** ⇒ 预判必须放行 —— 预判明说「大概率写得进去（但不保证）」，
	//     真正拦人的是写路径的事务内检查（它知道这次写入有多大，预判不知道）。
	h, herr := d.MemoryIndexHeadroom(scope)
	if herr != nil {
		t.Fatalf("读余量失败: %v", herr)
	}
	err := d.CheckMemoryIndexCap(scope)
	var capErr *MemoryCapError
	if h.EntriesFull || h.BytesFull {
		if !errors.As(err, &capErr) {
			t.Fatalf("已满的 scope 的预判错误不是 *MemoryCapError：%v", err)
		}
		if !errors.Is(err, ErrMemoryIndexFull) {
			t.Error("预判错误必须 unwrap 到 ErrMemoryIndexFull")
		}
		return
	}
	if err != nil {
		t.Fatalf("还有余量（条目 %d/%d，字节 %d/%d）却报了预判错误：%v",
			h.Entries, MemoryIndexMaxEntries, h.Bytes, MemoryIndexMaxBytes, err)
	}
}

// strconvItoa 是本文件用的整数转字符串（小工具，避免为一个转换引 strconv）。
func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
