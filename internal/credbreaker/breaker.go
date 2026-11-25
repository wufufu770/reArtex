// Package credbreaker 实现凭据维度的熔断器（A-13，0.15.82）：
// 同一凭据（profile + 目标 host）连续 2 次收到 401/403 后断路 30 分钟，
// 期间调用方在发请求前查 Allow 即可拿到确定性拒绝——不再重试撞锁。
//
// 与 llmpool.Registry（按 profile 计、冷却阶梯、软/硬失败分档）互补，不替代：
// 这里的键是（profile + host），策略固定为 2×401/403 → 30min。安全阈值是
// 代码常量，不进 settings——一次 PUT /api/settings 不能改掉熔断语义
// （与 pathguard 的 secretRoots 同款原则）。
//
// 持久化走 persist/forget 回调（模式同 llmpool/health.go）：进程内状态是权威，
// DB 是镜像；启动期 Load + Restore 让冷却窗跨重启存活。onOpen 供宿主在断路
// 瞬间记审计/activity，nil = 只落库。
package credbreaker

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// 策略常量：连续 2 次 401/403 → 断路 30 分钟。
const (
	MaxConsecutiveAuthFailures = 2
	OpenDuration               = 30 * time.Minute
)

// Key 是熔断的凭据维度。Host 归一化为小写（内部再做一次，调用方无需保证）；
// 空 host = 该凭据的全局维度（目标不明时的兜底键）。
type Key struct {
	ProfileID int64
	Host      string
}

// State 是一个键的熔断状态。
type State struct {
	Consecutive int       // 连续 401/403 计数（成功即清零）
	OpenUntil   time.Time // 零值/过去 = 未断路
	LastReason  string    // 最近一次计数原因，如 "HTTP 401"
	LastAt      time.Time
}

// Open 报告是否处于断路窗口内。
func (s State) Open() bool { return time.Now().Before(s.OpenUntil) }

type breakerState = State // 内部别名，减少指针解引用噪音

// Breaker 是进程级共享的凭据熔断器。零值不可用，经 New 构造。
type Breaker struct {
	mu     sync.RWMutex
	states map[Key]*breakerState

	// persist / forget 镜像到 DB（回调可能为 nil；宿主保证异步、尽力而为）。
	persist func(Key, State)
	forget  func(Key)
	// onOpen 在断路瞬间调用（锁外），宿主据此记审计事件与 activity。nil = 跳过。
	onOpen func(Key, State)
}

// New 构造熔断器。persist/forget/onOpen 均可为 nil（纯内存模式，测试用）。
func New(persist func(Key, State), forget func(Key), onOpen func(Key, State)) *Breaker {
	return &Breaker{
		states:  map[Key]*breakerState{},
		persist: persist,
		forget:  forget,
		onOpen:  onOpen,
	}
}

// Restore 启动期从 DB 播种状态（绕过持久化回调——数据本来就来自 DB）。
func (b *Breaker) Restore(k Key, st State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := st
	b.states[normKey(k)] = &cp
}

// Allow 报告该凭据当前是否可发起请求。断路窗口内 false。
func (b *Breaker) Allow(k Key) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	st, ok := b.states[normKey(k)]
	return !ok || !st.Open()
}

// OpenUntil 返回该键的断路截止时刻（未断路时为零值），供拒绝信息给出可操作的
// 「何时重试」。
func (b *Breaker) OpenUntil(k Key) time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()
	st, ok := b.states[normKey(k)]
	if !ok {
		return time.Time{}
	}
	return st.OpenUntil
}

// Record 记一次带状态码的调用结果：
//   - 401/403：连续计数 +1；达到 2 即断路 30 分钟（返回 true = 本次调用触发断路，
//     调用方可据此只记一条日志/审计）；
//   - 2xx：复位计数（只有「连续」的 401/403 才累积，成功一次即重新信任）；
//   - 其他（429/5xx/网络）：不改计数——偶发非凭据失败不推进也不清零凭据熔断。
func (b *Breaker) Record(k Key, statusCode int) (tripped bool) {
	k = normKey(k)
	b.mu.Lock()
	st := b.states[k]
	if st == nil {
		st = &breakerState{}
		b.states[k] = st
	}
	now := time.Now()
	switch {
	case statusCode == 401 || statusCode == 403:
		// 已断路的键继续吃 401/403 不再重复断路：窗口内拒绝由 Allow 承担。
		if st.Open() {
			b.mu.Unlock()
			return false
		}
		st.Consecutive++
		st.LastReason = "HTTP " + strconv.Itoa(statusCode)
		st.LastAt = now
		if st.Consecutive >= MaxConsecutiveAuthFailures {
			st.OpenUntil = now.Add(OpenDuration)
			tripped = true
		}
	case statusCode >= 200 && statusCode < 300:
		delete(b.states, k)
		forget := b.forget
		b.mu.Unlock()
		if forget != nil {
			forget(k)
		}
		return false
	default:
		b.mu.Unlock()
		return false
	}
	snap := *st
	persist, onOpen := b.persist, b.onOpen
	b.mu.Unlock()
	if persist != nil {
		persist(k, snap)
	}
	if tripped && onOpen != nil {
		onOpen(k, snap)
	}
	return tripped
}

// Reset 清除一个键的状态——宿主的「立即恢复/更换凭据后复位」入口。
func (b *Breaker) Reset(k Key) {
	k = normKey(k)
	b.mu.Lock()
	delete(b.states, k)
	forget := b.forget
	b.mu.Unlock()
	if forget != nil {
		forget(k)
	}
}

// Snapshot 返回全部状态的拷贝（metrics/status API 用）。
func (b *Breaker) Snapshot() map[Key]State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[Key]State, len(b.states))
	for k, st := range b.states {
		out[k] = *st
	}
	return out
}

// normKey 归一化键：host 小写。空 host 保持为空（全局维度）。
func normKey(k Key) Key {
	k.Host = strings.ToLower(strings.TrimSpace(k.Host))
	return k
}
