package agent

// loopguard（0.15.34，批次 F）：重复调用拦截——同签名工具调用计数，
// 3× 硬拦 + 拦截 4 次后拒绝消息升级。
//
// 为什么工具层拦截而提示词已有「封锁路线不无理由重试」纪律：提示词约束是软的
// （Worse Together 实证：prompt 规则拦不住）；签名级硬拦是确定性的。阈值挂源码
// 常量（不进热改面，与 shortcircuit 同纪律）。
//
// 与 SoD 链 deniedRecently 的分工：那是「同 payload 刚被拒过」的拒绝面短路；
// 本守卫是「同 payload 反复提交执行」的提交面拦截，不重叠。
//
// 振荡检测（A→B→A→B）v1 刻意不做：振荡对渗透工作流是正常形态（两个探测交替
// 是常见节奏），误拦率不可控；相同签名 3× 才是纯空转信号。留档不移植。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

const (
	// LoopGuardBlockThreshold 是同一签名触发硬拦的次数（第 3 次调用即拦）。
	LoopGuardBlockThreshold = 3
	// LoopGuardEscalateAfter 是累计硬拦达到该数后，拒绝消息升级为收尾指引
	// （模型连续撞墙说明本轮策略整体失效，该收尾输出结论而不是继续撞）。
	LoopGuardEscalateAfter = 4
)

// LoopVerdict 是守卫的判定。
type LoopVerdict struct {
	Block   bool
	Message string
}

// LoopGuard 是一次 run 的重复调用守卫（per-run 状态；内部锁防御性——hooks
// 链是串行调用）。
type LoopGuard struct {
	mu      sync.Mutex
	sigs    map[string]int
	blocked int
}

// NewLoopGuard 建空守卫。
func NewLoopGuard() *LoopGuard { return &LoopGuard{sigs: map[string]int{}} }

// Observe 记一次工具调用并判定。签名 = 工具名 + 参数键排序 canonical 的
// SHA-256（时间戳/请求 id 等易变字段不在键序里——它们在 JSON 里就是值，
// 排序不消除易变值本身；渗透工具的重复调用参数通常逐字节相同，签名稳定）。
func (g *LoopGuard) Observe(tool string, input []byte) LoopVerdict {
	sig := loopSignature(tool, input)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sigs[sig]++
	n := g.sigs[sig]
	if n < LoopGuardBlockThreshold {
		return LoopVerdict{Block: false}
	}
	g.blocked++
	if g.blocked >= LoopGuardEscalateAfter {
		return LoopVerdict{Block: true, Message: fmt.Sprintf(
			"[loopguard] 该调用已第 %d 次重复执行且已被拦 %d 次——本轮策略整体失效，请停止重试，直接输出已取得的结论收尾。", n, g.blocked)}
	}
	return LoopVerdict{Block: true, Message: fmt.Sprintf(
		"[loopguard] 相同调用已重复 %d 次（第 %d 次被拦）：换方法、换参数、换路径，或确认该方向已探尽后转下一方向。不要原样重试。", n, g.blocked)}
}

// loopSignature = 工具名 + 参数键排序 canonical 的 SHA-256。
func loopSignature(tool string, input []byte) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(loopCanonicalJSON(input))
	return hex.EncodeToString(h.Sum(nil))
}

// loopCanonicalJSON 输出键排序 compact JSON（与 norma 语义一致足够——签名
// 只需要确定性，不需要与 Python 对齐）。
func loopCanonicalJSON(input []byte) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(input))
	if err := dec.Decode(&v); err != nil {
		// 非 JSON 参数（罕见）：原文即签名输入。
		return input
	}
	var buf bytes.Buffer
	loopEncode(&buf, v)
	return buf.Bytes()
}

// loopEncode 递归输出键排序 compact JSON。
func loopEncode(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			le, _ := json.Marshal(k)
			buf.Write(le)
			buf.WriteByte(':')
			loopEncode(buf, t[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			loopEncode(buf, e)
		}
		buf.WriteByte(']')
	default:
		le, _ := json.Marshal(v)
		buf.Write(le)
	}
}
