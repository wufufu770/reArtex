package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Autumn-27/artex/internal/credbreaker"
	"github.com/Autumn-27/norma/llm"
)

// Member is one LLM profile in the chain, already built into a provider (wrapped
// with the recorder by the caller, so a failed attempt is still recorded under
// its own profile name).
type Member struct {
	ID       int64  // llm_profiles.id
	Name     string // profile name, for logs / UI
	Model    string
	Format   string // "anthropic" | "openai"
	Priority int    // the profile's configured priority (display only)
	Active   bool   // is_default (display only)
	// Rank is the ordering key the caller assigned: higher goes first, and members
	// sharing a Rank take turns leading (load-spreading across duplicate keys).
	// The caller encodes "active profile heads the chain" as a Rank above every
	// user-settable priority, so this type needs no policy of its own.
	Rank int
	// WindowTokens is the profile's context window in tokens. A member whose
	// window can't hold the request is skipped rather than made to fail on it.
	WindowTokens int
	// Host 是该 profile 端点的主机名（来自 BaseURL，A-13）：凭据熔断器按
	// （profile + host）计。空 = 端点未明（SDK 默认地址），退化为 profile 全局维度。
	Host string
	Prov llm.Provider
}

// HostOf 从 BaseURL 提取归一化主机名（A-13，宿主侧构造 Member 时用）。
// 解析失败/为空返回 ""（退化为 profile 全局维度）。
func HostOf(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// RankActive is the Rank the caller gives the chain head (the active profile, or
// an explicitly bound one) so it always outranks any configured priority.
const RankActive = int(^uint(0)>>1) - 1

// ErrExhausted is returned when every member of the chain failed.
var ErrExhausted = errors.New("LLM 轮询：所有配置均不可用")

// Pool is an llm.Provider that fails over across an ordered chain of members.
// It is safe for concurrent use: members are immutable after construction and
// all mutable state lives in the shared Registry.
type Pool struct {
	members []*Member // in chain order (active first, then priority DESC)
	health  *Registry
	rr      atomic.Uint64 // rotates the starting point within an equal-priority group
	// cred 是凭据维度熔断器（A-13，键 = profile+host）。nil = 未接（单测/降级），
	// 行为与未接时完全一致。它与 health 互补：health 管「这个 profile 还能用吗」，
	// cred 管「这份凭据对这个端点 host 还要不要再撞」。
	cred *credbreaker.Breaker
	// OnHardTrip 是硬熔断（key 失效/配额耗尽/配置缺失）的宿主回调（v5.3 G-B）：
	// 宿主据此群播报处置指引。kind 是失败类别（HardKindAuth/Quota/Missing），
	// 宿主按类别给准确措辞——0.15.9 修复：此前一律说「鉴权失效请轮换 Key」，
	// 429 配额耗尽也这么喊，误导运维。nil = 只留日志。
	OnHardTrip func(memberName, errMsg, kind string)
	// OnFailover 是每次成员切换的宿主回调（v5.3 G-E 可观测：切换原因/
	// 从谁到谁，全部留痕+播报）。nil = 只留日志。
	OnFailover func(fromName, toName, reason string)
}

// New builds a Pool over members (already in chain order). cred 为凭据熔断器
// （A-13），可为 nil。Returns nil when the chain is empty. A single-member chain
// is still a valid Pool — it just behaves exactly like the bare provider.
func New(members []*Member, health *Registry, cred *credbreaker.Breaker) *Pool {
	if len(members) == 0 {
		return nil
	}
	if health == nil {
		health = NewRegistry(nil, nil)
	}
	return &Pool{members: members, health: health, cred: cred}
}

// credKey / credAllow / credRecord 是凭据熔断器的 nil 安全封装（A-13）。
// record 在断路瞬间补一条进程日志——审计/activity 由 Breaker 的 onOpen 回调
// 在宿主（server）侧记，这里不重复。
func (p *Pool) credKey(m *Member) credbreaker.Key {
	return credbreaker.Key{ProfileID: m.ID, Host: m.Host}
}

func (p *Pool) credAllow(m *Member) bool {
	return p.cred == nil || p.cred.Allow(p.credKey(m))
}

func (p *Pool) credRecord(m *Member, statusCode int) {
	if p.cred == nil {
		return
	}
	if p.cred.Record(p.credKey(m), statusCode) {
		log.Printf("[llmpool] 凭据熔断：%s 对 %s 连续 %d 次 401/403，断路 30 分钟（更换凭据或等待后自动恢复）",
			m.Name, m.Host, credbreaker.MaxConsecutiveAuthFailures)
	}
}

// Members returns the chain in order (read-only).
func (p *Pool) Members() []*Member { return p.members }

// Head returns the first member of the chain.
func (p *Pool) Head() *Member { return p.members[0] }

// Stream implements llm.Provider with failover.
//
// The one hard rule: a member may only be abandoned BEFORE it has yielded any
// event. Once text or a tool_use has reached the caller, re-sending the same
// request to another model would duplicate output and corrupt the conversation
// history — so a mid-stream failure is surfaced as-is and left to the agent
// harness's resume logic. Fortunately the failures this exists for (402 no
// credit, 401 bad key, 429, 5xx) all surface during request establishment,
// before the body is read, so they always land in the safe window.
func (p *Pool) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	order := p.order(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		var lastErr error
		for i, m := range order {
			emitted := false
			var failed error
			// thinking_fallback（0.15.44 #6）：400 且请求带 thinking/reasoning 参数
			// → 剥离后在本成员上重放一次（安全窗口内零输出已发）。参数是宿主加的，
			// 网关不支持时剥离重放是宿主权责；重放仍 400 则原样上抛（不 trip health，
			// 与既有 400 语义一致）。
			for attempt := 0; attempt < 2; attempt++ {
				var retried, terminalTripped bool
				var terminal error
			streamLoop:
				for ev, err := range m.Prov.Stream(ctx, req) {
					if err != nil && !emitted {
						if !retried && attempt == 0 && statusOf(err) == 400 {
							if stripped, ok := stripThinkingParams(req); ok {
								req = stripped
								retried = true
								log.Printf("[llmpool] thinking_fallback：%s 400（疑似 thinking 参数不兼容），剥离 thinking/effort 参数重放一次", m.Model)
								break streamLoop
							}
						}
						if shouldFailover(ctx, err) {
							failed = err
						} else {
							terminal = err
							terminalTripped = true
						}
						break streamLoop
					}
					emitted = true
					if !yield(ev, err) {
						return // caller stopped consuming (cancel / early exit)
					}
					if err != nil {
						return // terminal error already handed to the caller
					}
				}
				if retried {
					continue // attempt 2：以剥参请求重放本成员
				}
				if terminalTripped {
					// 非 failover 终止（400 无参可剥 / ctx 取消等）：原样交给调用方。
					p.health.Pass(m.ID)
					yield(llm.StreamEvent{}, terminal)
					return
				}
				break
			}
			if failed == nil {
				p.health.Pass(m.ID)  // completed (or failed in a non-failover way)
				p.credRecord(m, 200) // 成功即复位凭据计数（A-13）
				return
			}
			lastErr = failed
			p.credRecord(m, statusOf(failed)) // 401/403 计数，其余码不改（A-13）
			hard := isHardFailure(failed)
			if p.health.Trip(m.ID, trimErr(failed), hard) {
				log.Printf("[llmpool] 配置 %q(%s) 已熔断：%s", m.Name, m.Model, trimErr(failed))
				if hard && p.OnHardTrip != nil {
					p.OnHardTrip(m.Name+"/"+m.Model, trimErr(failed), hardKind(failed))
				}
			}
			if i+1 < len(order) {
				n := order[i+1]
				log.Printf("[llmpool] LLM 故障转移：%q(%s) → %q(%s)，原因：%s",
					m.Name, m.Model, n.Name, n.Model, trimErr(failed))
				if p.OnFailover != nil {
					p.OnFailover(m.Name+"/"+m.Model, n.Name+"/"+n.Model, trimErr(failed))
				}
			}
		}
		if lastErr == nil {
			lastErr = ErrExhausted
		}
		log.Printf("[llmpool] 轮询链已耗尽(%d 个配置全部失败)，最后错误：%s", len(order), trimErr(lastErr))
		yield(llm.StreamEvent{}, fmt.Errorf("%w：%v", ErrExhausted, lastErr))
	}
}

// Complete implements llm.Provider with failover for non-streaming calls. A
// non-streaming request is atomic — it never delivers partial output — so every
// failover-eligible failure lands in the safe window and the next member can be
// tried without risk of duplicated output. Mirrors Stream's health-tripping and
// chain-exhaustion behavior.
func (p *Pool) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	order := p.order(req)
	var lastErr error
	for i, m := range order {
		msg, sr, usage, err := m.Prov.Complete(ctx, req)
		// thinking_fallback（0.15.44 #6）：400 且请求带 thinking/reasoning 参数
		// → 剥离重放一次（后续 failover 成员同样使用剥参请求）。
		if err != nil && statusOf(err) == 400 {
			if stripped, ok := stripThinkingParams(req); ok {
				log.Printf("[llmpool] thinking_fallback：%s 400（疑似 thinking 参数不兼容），剥离 thinking/effort 参数重放一次", m.Model)
				req = stripped
				msg, sr, usage, err = m.Prov.Complete(ctx, req)
			}
		}
		if err == nil {
			p.health.Pass(m.ID)
			p.credRecord(m, 200) // 成功即复位凭据计数（A-13）
			return msg, sr, usage, nil
		}
		if !shouldFailover(ctx, err) {
			// Non-failover error (e.g. ctx cancel, deterministic 4xx): surface as-is
			// without tripping health, matching Stream's non-failover path.
			p.health.Pass(m.ID)
			return llm.Message{}, "", llm.Usage{}, err
		}
		lastErr = err
		p.credRecord(m, statusOf(err)) // 401/403 计数，其余码不改（A-13）
		hard := isHardFailure(err)
		if p.health.Trip(m.ID, trimErr(err), hard) {
			log.Printf("[llmpool] 配置 %q(%s) 已熔断：%s", m.Name, m.Model, trimErr(err))
			if hard && p.OnHardTrip != nil {
				p.OnHardTrip(m.Name+"/"+m.Model, trimErr(err), hardKind(err))
			}
		}
		if i+1 < len(order) {
			n := order[i+1]
			log.Printf("[llmpool] LLM 故障转移：%q(%s) → %q(%s)，原因：%s",
				m.Name, m.Model, n.Name, n.Model, trimErr(err))
		}
	}
	if lastErr == nil {
		lastErr = ErrExhausted
	}
	log.Printf("[llmpool] 轮询链已耗尽(%d 个配置全部失败)，最后错误：%s", len(order), trimErr(lastErr))
	return llm.Message{}, "", llm.Usage{}, fmt.Errorf("%w：%v", ErrExhausted, lastErr)
}

// order picks the members to try, in order: skip those in a cooling-off window
// and those whose context window can't hold this request, then rotate within each
// equal-priority group so same-priority profiles share the load. Never returns an
// empty slice — if everything is filtered out, the head of the chain is tried
// anyway, since stalling the engine is worse than one more failed request.
func (p *Pool) order(req llm.CompletionRequest) []*Member {
	est := estimateTokens(req)
	var open []*Member
	for _, m := range p.members {
		if p.health.IsOpen(m.ID) {
			continue
		}
		if !p.credAllow(m) {
			continue // 凭据熔断中（A-13）：跳过该成员而不是再撞一次 401/403
		}
		if m.WindowTokens > 0 && est > m.WindowTokens {
			continue // would 400 on length — not a useful failover target
		}
		open = append(open, m)
	}
	if len(open) == 0 {
		return p.members[:1] // last resort: probe the head rather than stall
	}
	return rotateGroups(open, p.rr.Add(1)-1)
}

// rotateGroups rotates each run of equal-ranked members by n, so profiles sharing
// a priority take turns going first (free load-spreading across duplicate keys).
// The chain head has its own Rank and always stays at the front.
func rotateGroups(in []*Member, n uint64) []*Member {
	out := make([]*Member, 0, len(in))
	for i := 0; i < len(in); {
		j := i + 1
		for j < len(in) && in[j].Rank == in[i].Rank {
			j++
		}
		g := in[i:j]
		if len(g) > 1 {
			off := int(n % uint64(len(g)))
			for k := range g {
				out = append(out, g[(k+off)%len(g)])
			}
		} else {
			out = append(out, g...)
		}
		i = j
	}
	return out
}

// statusRe pulls the HTTP status out of the SDK's error text, which is formatted
// as "<prefix>: status <code>: <body>" (norma/llm/retry.go). The SDK exposes no
// typed error, so the string is what we have.
var statusRe = regexp.MustCompile(`status (\d{3})`)

// statusOf returns the HTTP status carried by err, or 0 if it isn't one.
func statusOf(err error) int {
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

// stripThinkingParams 返回剥离 thinking/reasoning 参数后的请求副本（0.15.44
// #6 thinking_fallback）。第二返回值 false = 请求本就不带这两个参数（无可剥离）。
func stripThinkingParams(req llm.CompletionRequest) (llm.CompletionRequest, bool) {
	if req.Thinking == "" && req.ReasoningEffort == "" {
		return req, false
	}
	r2 := req
	r2.Thinking = ""
	r2.ReasoningEffort = ""
	return r2, true
}

// shouldFailover reports whether err justifies trying the next profile.
//
// Never fails over on context cancellation — that's the user stopping a task or a
// task-level timeout, and burning a backup key on it would both waste credit and
// pollute the run's termination diagnosis. Never on 400 either: a malformed or
// over-long request fails identically everywhere.
func shouldFailover(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch code := statusOf(err); {
	case code == 0:
		return true // no status → transport-level failure (reset / DNS / timeout)
	case code == 400:
		return false // bad or over-long request: identical everywhere
	case code == 401, code == 402, code == 403, code == 404, code == 408, code == 429:
		return true
	case code >= 500:
		return true
	}
	return false
}

// isHardFailure reports whether the failure is deterministic (the profile will
// keep failing until a human fixes it) rather than transient. Hard failures open
// the breaker on the first occurrence.
//
// v0.9.17：配额耗尽型 429（「已达到 Token Plan 用量上限」/ quota / usage limit）
// 也算硬失败。这类 429 不是瞬时限流——套餐不充值就一直 429，按瞬态处理要凑满
// softTrip 次才熔断、且每次熔断冷却结束又浪费 softTrip 次请求去探测（task#9
// 「workers 空跑」的主因之一：05:33-05:51 每个请求都先撞一次 Minimax 429 再
// 转移）。按硬失败处理 = 首次即熔断，每个冷却窗只浪费一次半开探测。
func isHardFailure(err error) bool {
	switch statusOf(err) {
	case 401, 402, 403, 404:
		return true
	case 429:
		return isQuotaExhausted(err)
	}
	return false
}

// HardTrip 失败类别（OnHardTrip 的 kind 实参）：宿主据此给准确处置指引。
const (
	HardKindAuth    = "auth"    // 401/403 key 无效
	HardKindQuota   = "quota"   // 402 / 429 配额耗尽（区别于瞬时限流）
	HardKindMissing = "missing" // 404 模型/端点不存在
)

func hardKind(err error) string {
	switch statusOf(err) {
	case 401, 403:
		return HardKindAuth
	case 402:
		return HardKindQuota
	case 404:
		return HardKindMissing
	case 429:
		if isQuotaExhausted(err) {
			return HardKindQuota
		}
	}
	return ""
}

// quotaRe 匹配「配额/套餐耗尽」语义的响应体（区别于瞬时限流 rate limit）。
// 已知形态：MiniMax「已达到 Token Plan 用量上限」、OpenAI「insufficient_quota」、
// Anthropic「credit balance is too low」等。匹配不了就回落瞬态处理（保守：
// 误判硬会让恢复慢一个冷却窗，误判瞬态才是空跑放大器）。
var quotaRe = regexp.MustCompile(`(?i)(用量上限|额度|配额|usage limit|quota (exceeded|exhausted)|insufficient_quota|billing (hard )?limit|credit balance|exceeded your)`)

// isQuotaExhausted reports whether a 429 body reads as quota/plan exhaustion
// rather than transient rate limiting.
func isQuotaExhausted(err error) bool {
	return quotaRe.MatchString(err.Error())
}

// trimErr shortens an error for logs/UI — provider bodies can be long.
func trimErr(err error) string {
	s := strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// estimateTokens roughly sizes a request so members whose context window clearly
// can't hold it are skipped. Deliberately crude (~3.5 chars/token) and biased to
// over-estimate slightly; it only needs to tell "fits" from "nowhere near".
func estimateTokens(req llm.CompletionRequest) int {
	n := 0
	for _, s := range req.System {
		n += len(s)
	}
	for _, m := range req.Messages {
		n += blocksLen(m.Content)
	}
	for _, t := range req.Tools {
		// InputSchema is a decoded map, so its serialized size isn't available
		// cheaply — charge a flat ~120 chars per top-level property instead.
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)*120
	}
	return n * 2 / 7 // ≈ len/3.5
}

func blocksLen(bs []llm.ContentBlock) int {
	n := 0
	for _, b := range bs {
		n += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Name)
		if len(b.Content) > 0 {
			n += blocksLen(b.Content)
		}
	}
	return n
}
