package sod

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// TokenStore 是进程内的一次性执行凭证存储。
//
// 为什么**不进 DB**：凭证是秒级寿命的一次性票据；DB 行可以被 DELETE，
// 凭证被删 = 放行（攻击者删一行就能把「批准过」变成「没批准过」的相反面）。
// 进程内 + 原子消费 = 单一真相源。
//
// 两个索引（同一张表的两种查法）：
//
//   - tokens：token 本体 ⇒ 凭证（显式持 token 的调用方走这条）
//   - byTuple：(载荷摘要, 任务, agent, 工具) ⇒ token 本体（执行器走这条 ——
//     hook 的返回契约里**没有地方可以回传 token**，见计划「口径统一」第 3 条）
type TokenStore struct {
	mu      sync.Mutex
	tokens  map[string]*executionToken
	byTuple map[string]string
}

// executionToken 是一枚未消费的凭证。
type executionToken struct {
	payloadDigest string
	taskID        int64
	sourceAgent   string
	toolName      string
	digest        string
	expiresAt     time.Time
}

// NewTokenStore 建一个空的凭证表。
func NewTokenStore() *TokenStore {
	return &TokenStore{
		tokens:  map[string]*executionToken{},
		byTuple: map[string]string{},
	}
}

// Mint 铸一枚凭证：绑定 (payloadDigest, taskID, sourceAgent, toolName)。
//
// 返回 token 本体与它的摘要。**token 本体只出现这一次** —— 不落库、不进日志、
// 不进模型上下文；只有摘要会进审计（查回审计能证明「批准过」，但拿不到凭证本身）。
//
// 载荷摘要绑定的是**将要执行的那份字节**的摘要（不是审批时看到的字节）——
// 这是 TOCTOU 窗口的唯一防线：窗口里入参被换掉，执行器会因摘要不匹配而拒绝。
//
// 同一个四元组在同一时刻只允许存在一枚未消费的凭证：这里先作废旧的再写新的
// （旧的若是已过期的，正好被清掉；旧的若还没过期，它对应的批准已经作废了 ——
// 同一份载荷被重新批准一次，旧凭证没有理由继续有效）。
func (s *TokenStore) Mint(payloadDigest string, taskID int64, sourceAgent, toolName string) (string, string, error) {
	raw := make([]byte, sodpolicy.TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成执行凭证失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	key := tupleKey(payloadDigest, taskID, sourceAgent, toolName)

	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byTuple[key]; ok {
		delete(s.tokens, old)
	}
	s.tokens[token] = &executionToken{
		payloadDigest: payloadDigest,
		taskID:        taskID,
		sourceAgent:   sourceAgent,
		toolName:      toolName,
		digest:        digest,
		expiresAt:     time.Now().Add(time.Duration(sodpolicy.TokenTTLSeconds) * time.Second),
	}
	s.byTuple[key] = token
	return token, digest, nil
}

// Consume 校验并**原子消费**一枚凭证（显式持 token 的调用场景）。
//
// 四项必须全对：token 存在、taskID 相同、sourceAgent 相同、toolName 相同、
// payloadDigest 相同。校验通过 ⇒ 删除 ⇒ 返回 true。
//
// ⚠️ 校验失败**不删除**凭证 —— 删了会让真正的持有者永远用不了（摘要算错的
// 那一侧不该有能力作废另一侧的批准）。
//
// ⚠️ 二次使用 ⇒ 查不到 ⇒ false ⇒ 执行器拒绝 ⇒ **重放失败**。
func (s *TokenStore) Consume(token, payloadDigest string, taskID int64, sourceAgent, toolName string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(t.expiresAt) {
		s.removeLocked(token, t)
		return false
	}
	if t.payloadDigest != payloadDigest || t.taskID != taskID ||
		t.sourceAgent != sourceAgent || t.toolName != toolName {
		return false
	}
	s.removeLocked(token, t)
	return true
}

// Take 按四元组取走并消费一枚凭证（执行器走这条 —— 它拿不到 token 本体）。
//
// 四元组任一不符 ⇒ 查不到 ⇒ false ⇒ **拒绝执行**。这同时保证了不可转让：
// A 任务的凭证拿到 B 任务 / B agent / 另一个工具名下用，四元组对不上。
//
// 载荷摘要也在四元组里 ⇒ 「为载荷 A 批的凭证」不能授权载荷 B。
func (s *TokenStore) Take(payloadDigest string, taskID int64, sourceAgent, toolName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tupleKey(payloadDigest, taskID, sourceAgent, toolName)
	token, ok := s.byTuple[key]
	if !ok {
		return false
	}
	t, ok := s.tokens[token]
	if !ok {
		// 索引与实体不一致（理论上不会发生）⇒ 清掉这条悬空索引。
		delete(s.byTuple, key)
		return false
	}
	if time.Now().After(t.expiresAt) {
		s.removeLocked(token, t)
		return false
	}
	s.removeLocked(token, t)
	return true
}

// Sweep 清理过期凭证，返回清掉的数量。
//
// TTL 的唯一目的是兜住「批准了但没执行」的窗口（工具被上层取消、进程被打断）。
// 它不是为了让凭证可以事后重放 —— 单次有效由「消费即删除」保证。
func (s *TokenStore) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for token, t := range s.tokens {
		if now.After(t.expiresAt) {
			s.removeLocked(token, t)
			removed++
		}
	}
	return removed
}

// removeLocked 删掉一枚凭证及其四元组索引（调用方持锁）。
func (s *TokenStore) removeLocked(token string, t *executionToken) {
	delete(s.tokens, token)
	key := tupleKey(t.payloadDigest, t.taskID, t.sourceAgent, t.toolName)
	if cur, ok := s.byTuple[key]; ok && cur == token {
		delete(s.byTuple, key)
	}
}

// tupleKey 拼出四元组索引键。
//
// 用 \x00 分隔：四个分量都是自由文本（agent key、工具名），不带分隔符的话
// ("a", "bc") 与 ("ab", "c") 会撞成同一个键 ⇒ 凭证就能跨维度转移。
func tupleKey(payloadDigest string, taskID int64, sourceAgent, toolName string) string {
	return payloadDigest + "\x00" + strconv.FormatInt(taskID, 10) + "\x00" + sourceAgent + "\x00" + toolName
}
