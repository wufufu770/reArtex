package server

// ---------- 网关候选地址自动切换（方案 v5 G1；T4 方案 v10 §6 接入 X8 触发）----------
//
// 一个 LLM profile 可以配 base_urls（有序候选，第一项为主）。装饰器持有每个
// 候选的独立 provider（各自带完整包装链：llmrec/quota/side——切换后记录与
// 额度照跑），请求失败且为【连接类】错误时切换下一候选重试，主地址恢复后
// 自动回切。切换全程留痕（切换历史 ring + llm_records 观测行）。
//
// 边界口径：
//   - 【连接类】失败切换照旧（G1）；T4（X8）追加：额度/认证失败在首个输出事件
//     前也允许换端点重派——同一模型挂多个网关时，额度与 key 有效性按网关各自
//     独立。重派上限：额度 2 次、认证 1 次（连续口径，成功即清零；常量与判定
//     在 role_policy.go，lane 侧共用）。401/403 的链级熔断仍由 llmpool 的
//     hard-trip 分类处理（G5），与本装饰器正交；
//   - 每次 Stream/Complete 调用最多把候选列表遍历一圈，不无限重试；
//   - 探测是 TCP dial（短超时）——「端口通」即候选可用，协议错误留给真实请求；
//   - 本装饰器此前是死代码（无装配点），T4 在 providerForProfileModelRaw
//     （唯一构建源）接入消费——见文件尾 endpointFailoverFor。

import (
	"context"
	"fmt"
	"iter"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// endpointSwitch 是一次端点切换的历史行。
type endpointSwitch struct {
	At    time.Time `json:"at"`
	From  string    `json:"from"`
	To    string    `json:"to"`
	Cause string    `json:"cause"`
}

// endpointFailover 是 llm.Provider 装饰器：候选间连接类失败的自动切换。
type endpointFailover struct {
	mu          sync.Mutex
	profileID   int64
	profileName string
	endpoints   []string         // 主在前（与 profile 配置顺序一致）
	provs       []llm.Provider   // 与 endpoints 一一对应
	cur         int              // 当前生效下标
	switches    []endpointSwitch // 最近切换历史（ring，最多 50）
	onSwitch    func(from, to, cause string)
	// T4（X8）：额度/认证失败的连续重派计数（成功一次清零；上限见
	// laneRedeployClass.capOf——与 role lane 同一份常量）。
	quotaRedeploys int
	authRedeploys  int
}

// buildEndpointProviders 为 profile 的每个候选 URL 构建独立 provider
// （buildOne 由宿主注入：cfg 改 BaseURL 后走完整的构建+包装链）。
func buildEndpointProviders(urls []string, buildOne func(baseURL string) (llm.Provider, error)) ([]llm.Provider, int) {
	provs := make([]llm.Provider, 0, len(urls))
	alive := 0
	for _, u := range urls {
		prov, err := buildOne(u)
		if err != nil {
			log.Printf("[llm-failover] 候选 %s 构建失败（跳过）: %v", u, err)
			provs = append(provs, nil)
			continue
		}
		provs = append(provs, prov)
		alive++
	}
	return provs, alive
}

// newEndpointFailover 组装装饰器。候选数 <2 或可用 provider <2 时返回 nil
// （调用方回落单 provider，行为与现状一致）。
func newEndpointFailover(profileID int64, profileName string, urls []string, provs []llm.Provider, onSwitch func(from, to, cause string)) *endpointFailover {
	alive := 0
	for _, pr := range provs {
		if pr != nil {
			alive++
		}
	}
	if len(urls) < 2 || alive < 2 {
		return nil
	}
	f := &endpointFailover{profileID: profileID, profileName: profileName, endpoints: urls, provs: provs, cur: 0, onSwitch: onSwitch}
	f.onSwitch = func(from, to, cause string) {
		log.Printf("[llm-failover] profile %d（%s）端点切换：%s → %s（%s）", profileID, profileName, from, to, cause)
		if onSwitch != nil {
			onSwitch(from, to, cause)
		}
	}
	return f
}

func trimEndpoint(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// current 返回当前生效候选（跳过构建失败的 nil 槽位）。
func (f *endpointFailover) current() (llm.Provider, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.provs)
	for step := 0; step < n; step++ {
		i := (f.cur + step) % n
		if f.provs[i] != nil {
			return f.provs[i], f.endpoints[i]
		}
	}
	return nil, ""
}

// switchTo 切换到指定下标（留痕；同下标幂等）。
func (f *endpointFailover) switchTo(idx int, cause string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx < 0 || idx >= len(f.endpoints) || idx == f.cur || f.provs[idx] == nil {
		return
	}
	from := f.endpoints[f.cur]
	f.cur = idx
	f.switches = append(f.switches, endpointSwitch{At: time.Now(), From: from, To: f.endpoints[idx], Cause: clipEndpoint(cause, 200)})
	if len(f.switches) > 50 {
		f.switches = f.switches[len(f.switches)-50:]
	}
	if f.onSwitch != nil {
		f.onSwitch(from, f.endpoints[idx], cause)
	}
}

// nextAlive 返回「当前之后下一个可用候选」下标；绕一圈没有则 -1。
func (f *endpointFailover) nextAlive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.provs)
	for step := 1; step <= n; step++ {
		i := (f.cur + step) % n
		if f.provs[i] != nil {
			return i
		}
	}
	return -1
}

// ---------- T4（方案 v10 §6 / X8）：额度/认证失败的端点重派 ----------
//
// 一个 profile 配多个 base_urls 的常见形态是「同一模型挂在多个网关/中转上」，
// 额度与 key 的有效性按网关各自独立 ⇒ 连接类之外，额度/认证失败也值得换下一
// 候选试一次。重派上限（代码常量，照 tier 纪律，与 role_policy lane 侧同一
// 方案条目 X8）：额度 2 次、认证 1 次；任一次成功调用后计数清零（上限约束的
// 是「连续失败连击」，不是实例生命周期总量）。端点切换不改模型——「返回模型
// 恒 ≠ 当前模型」的换模型通道在 role lane 的 backup（role_policy.go），这里
// 的不变量是「返回端点恒 ≠ 当前端点」（switchTo 的 idx != cur 保证）。

// redeployAllowed 报告该错误是否允许换端点重派：连接类恒可（仍受单次调用
// 遍历一圈约束）；额度/认证按分类上限（连续口径）；其余（业务错误、上下文
// 过长、确定性 4xx）换端点也没用，透传。
func (f *endpointFailover) redeployAllowed(err error) bool {
	if isConnectError(err) {
		return true
	}
	class, ok := classifyLaneFailure(err)
	if !ok {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	used := f.quotaRedeploys
	if class == redeployAuth {
		used = f.authRedeploys
	}
	return used < class.capOf()
}

// noteRedeploy 记一次按分类的端点重派（bump 计数）。
func (f *endpointFailover) noteRedeploy(err error) {
	class, ok := classifyLaneFailure(err)
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if class == redeployAuth {
		f.authRedeploys++
	} else {
		f.quotaRedeploys++
	}
}

// noteSuccess 清零连续失败计数（一次成功调用说明当前候选已恢复）。
func (f *endpointFailover) noteSuccess() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaRedeploys, f.authRedeploys = 0, 0
}

// redeploy 切到「当前之后下一个可用候选」；没有可用候选返回 false。
func (f *endpointFailover) redeploy(cause string) bool {
	next := f.nextAlive()
	if next < 0 {
		return false
	}
	f.switchTo(next, cause)
	return true
}

// streamWithFailover 是 Complete 用的执行循环：连接类失败切下一候选重试整个
// 请求（最多遍历一圈）；额度/认证失败按 X8 上限重派；其它错误原样透传。
// （Stream 有自己的循环——流式错误从事件序列里冒出来，generic attempt 的
// 返回值看不到它，见下方 streamOnce。）
func streamWithFailover[T any](f *endpointFailover, attempt func(prov llm.Provider) (T, bool, error)) (T, error) {
	var zero T
	for round := 0; round < len(f.endpoints); round++ {
		prov, _ := f.current()
		if prov == nil {
			return zero, fmt.Errorf("llm-failover: 无可用候选端点")
		}
		res, started, err := attempt(prov)
		if err == nil {
			f.noteSuccess()
			return res, nil
		}
		if started || !f.redeployAllowed(err) {
			return zero, err
		}
		cause := "连接失败：" + err.Error()
		if _, ok := classifyLaneFailure(err); ok {
			f.noteRedeploy(err)
			cause = "额度/认证失败：" + err.Error()
		}
		if !f.redeploy(cause) {
			return zero, err
		}
	}
	return zero, fmt.Errorf("llm-failover: 所有候选端点均连接失败")
}

// streamOnce 消费一次候选的流：事件原样 yield；首个错误若发生在任何输出
// 交付之前且允许重派（连接类 / 额度认证类未超上限），返回 retryable=true
// 与切换原因，让调用方换候选重放整个请求；否则错误原样交付并终止。
func (f *endpointFailover) streamOnce(prov llm.Provider, ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) (cause string, retryable bool) {
	delivered := false
	for ev, err := range prov.Stream(ctx, req) {
		if err == nil {
			if !yield(ev, nil) {
				return "", false
			}
			delivered = true
			continue
		}
		if !delivered && f.redeployAllowed(err) {
			if _, laneClass := classifyLaneFailure(err); laneClass {
				f.noteRedeploy(err)
				return "额度/认证失败：" + err.Error(), true
			}
			return "连接失败：" + err.Error(), true
		}
		yield(llm.StreamEvent{}, err)
		return "", false
	}
	f.noteSuccess()
	return "", false
}

// Stream 实现 llm.Provider：连接类失败自动换端点重放整个请求；额度/认证失败
// 在首个输出事件前按 X8 上限换端点重放；任何输出交付给调用方之后，错误一律
// 原样透传（重放会重复模型输出/工具副作用）。单次 Stream 调用最多把候选列表
// 遍历一圈，不无限重试。
func (f *endpointFailover) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for round := 0; round < len(f.endpoints); round++ {
			prov, _ := f.current()
			if prov == nil {
				yield(llm.StreamEvent{}, fmt.Errorf("llm-failover: 无可用候选端点"))
				return
			}
			cause, retryable := f.streamOnce(prov, ctx, req, yield)
			if !retryable {
				return
			}
			if !f.redeploy(cause) {
				yield(llm.StreamEvent{}, fmt.Errorf("llm-failover: 无可用候选端点可切换"))
				return
			}
		}
		yield(llm.StreamEvent{}, fmt.Errorf("llm-failover: 所有候选端点均失败"))
	}
}

// completeResult 是 Complete 泛型循环的产物包。
type completeResult struct {
	M llm.Message
	S string
	U llm.Usage
}

// Complete 实现 llm.Provider。
func (f *endpointFailover) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	res, err := streamWithFailover(f, func(prov llm.Provider) (completeResult, bool, error) {
		m, s, u, err := prov.Complete(ctx, req)
		return completeResult{M: m, S: s, U: u}, false, err
	})
	return res.M, res.S, res.U, err
}

// isConnectError 判定是否连接类失败（dial / DNS / 网络不可达 / 连接超时）。
func isConnectError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(net.Error); ok {
		return true
	}
	if uerr, ok := err.(*url.Error); ok {
		return isConnectError(uerr.Err)
	}
	s := err.Error()
	for _, marker := range []string{
		"dial tcp", "no such host", "network unreachable",
		"connection refused", "i/o timeout", "TLS handshake timeout",
		"proxyconnect tcp",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// StartEndpointProbing 启动主地址恢复回切探测（60s 周期，连续 3 次可达回切）。
func (f *endpointFailover) StartEndpointProbing(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		consecOK := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			f.mu.Lock()
			cur := f.cur
			main := f.endpoints[0]
			f.mu.Unlock()
			if cur == 0 {
				consecOK = 0
				continue
			}
			if probeEndpointTCP(main, 1500*time.Millisecond) {
				consecOK++
				if consecOK >= 3 {
					f.switchTo(0, "主地址恢复，自动回切")
					consecOK = 0
				}
			} else {
				consecOK = 0
			}
		}
	}()
}

// probeEndpointTCP 判定 endpoint 的 host:port 是否可连。
func probeEndpointTCP(endpoint string, d time.Duration) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	c, err := net.DialTimeout("tcp", host, d)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// recordEndpointSwitch 把切换写进 llm_records（观测通道；worker=llm-failover）。
func (s *Server) recordEndpointSwitch(profileID int64, name, from, to, cause string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	body := fmt.Sprintf(`{"event":"endpoint_switch","profile":%d,"profile_name":%q,"from":%q,"to":%q,"cause":%q}`,
		profileID, name, from, to, cause)
	if err := s.m.pg.InsertLLMRecord(&db.LLMRecord{
		Worker: "llm-failover", Model: name, Status: "ok",
		RawResponse: body,
	}); err != nil {
		log.Printf("[llm-failover] 切换留痕失败（忽略）: %v", err)
	}
}

// endpointFailoverFor 是 T4（方案 v10 §6 / X8）的消费接线：profile 配置了多个
// base_urls（≥2 个有效候选）时，把裸 provider 装配成 endpointFailover 装饰器
// 返回；单 base_url（或候选构建不出 ≥2 个可用 provider）时原样返回——行为与
// 现状逐字节一致（零变化）。切换留痕：log + llm_records 观测行。
//
// 装配位置在 providerForProfileModelRaw（唯一构建源）⇒ 所有下游出口（role
// lane / 任务链 / 任务 pin / auditor / normalizer / chat）都自动获得端点切换。
// 候选 provider 走 providerForProfileBareURL（改 BaseURL 的 Bare 构建：不写
// provByProfile 缓存、不触 poolForBinding）⇒ 不与 Pool 构建互递归，共享同一份
// profile 级限流器。探测回切 goroutine 挂 s.ctx（60s 周期；配置编辑重建实例后
// 旧探测最迟活到停机，代价仅周期性 TCP 探测）。
func (s *Server) endpointFailoverFor(id int64, p *db.LLMProfile, prov llm.Provider) llm.Provider {
	if s == nil || s.m == nil || s.m.pg == nil || p == nil || len(p.BaseURLs) < 2 {
		return prov
	}
	urls := make([]string, 0, len(p.BaseURLs))
	for _, u := range p.BaseURLs {
		if u = trimEndpoint(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) < 2 {
		return prov
	}
	provs, _ := buildEndpointProviders(urls, func(baseURL string) (llm.Provider, error) {
		cand, _, ok := s.providerForProfileBareURL(id, baseURL)
		if !ok {
			return nil, fmt.Errorf("候选 %s 构建失败", baseURL)
		}
		return cand, nil
	})
	f := newEndpointFailover(id, p.Name, urls, provs, func(from, to, cause string) {
		s.recordEndpointSwitch(id, p.Name, from, to, cause)
	})
	if f == nil {
		return prov
	}
	log.Printf("[llm-failover] profile %d（%s）装配端点切换（%d 个候选，X8：额度重派≤%d/认证重派≤%d）",
		id, p.Name, len(urls), LaneQuotaRedeployMax, LaneAuthRedeployMax)
	// 主地址恢复回切探测：60s 周期，随服务 ctx 终止（配置编辑会重建整个实例，
	// 旧实例的探测 goroutine 最迟活到停机——60s 一次 TCP 探测，代价可忽略）。
	f.StartEndpointProbing(s.ctx.Done())
	return f
}

// endpointStatus 是 /api/llm 的 endpoints 载荷行（健康卡）。
type endpointStatus struct {
	ProfileID  int64            `json:"profile_id"`
	Name       string           `json:"name"`
	Current    string           `json:"current"`
	Main       string           `json:"main"`
	Candidates []map[string]any `json:"candidates"`
	Switches   []endpointSwitch `json:"switches"`
}

// status 产出健康卡数据（候选探测短超时，最多 2s/个）。
func (f *endpointFailover) status() endpointStatus {
	f.mu.Lock()
	cur := f.cur
	eps := append([]string{}, f.endpoints...)
	n := len(f.provs)
	f.mu.Unlock()
	st := endpointStatus{ProfileID: f.profileID, Name: f.profileName, Main: eps[0], Candidates: make([]map[string]any, 0, len(eps))}
	if cur < len(eps) {
		st.Current = eps[cur]
	}
	for i, u := range eps {
		alive := i < n && probeEndpointTCP(u, 1200*time.Millisecond)
		st.Candidates = append(st.Candidates, map[string]any{
			"url": u, "alive": alive, "current": i == cur,
		})
	}
	f.mu.Lock()
	if len(f.switches) > 20 {
		st.Switches = append([]endpointSwitch{}, f.switches[len(f.switches)-20:]...)
	} else {
		st.Switches = append([]endpointSwitch{}, f.switches...)
	}
	f.mu.Unlock()
	return st
}

func clipEndpoint(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
