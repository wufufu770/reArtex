package server

// ---------- 网关候选地址自动切换（方案 v5 G1）----------
//
// 一个 LLM profile 可以配 base_urls（有序候选，第一项为主）。装饰器持有每个
// 候选的独立 provider（各自带完整包装链：llmrec/quota/side——切换后记录与
// 额度照跑），请求失败且为【连接类】错误时切换下一候选重试，主地址恢复后
// 自动回切。切换全程留痕（切换历史 ring + llm_records 观测行）。
//
// 边界口径：
//   - 只对【连接类】失败切换——401/403（鉴权）与业务错误与端点无关，照常透传
//     （401/403 的熔断由 llmpool 的 hard-trip 分类处理，见 G5）；
//   - 每次 Stream/Complete 调用最多把候选列表遍历一圈，不无限重试；
//   - 探测是 TCP dial（短超时）——「端口通」即候选可用，协议错误留给真实请求。

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

// streamWithFailover 是 Stream/Complete 共用的执行循环：连接类失败切下一
// 候选重试整个请求，最多遍历一圈；其它错误原样透传。
func streamWithFailover[T any](f *endpointFailover, attempt func(prov llm.Provider) (T, bool, error)) (T, error) {
	var zero T
	for round := 0; round < len(f.endpoints); round++ {
		prov, _ := f.current()
		if prov == nil {
			return zero, fmt.Errorf("llm-failover: 无可用候选端点")
		}
		res, started, err := attempt(prov)
		if err == nil {
			return res, nil
		}
		if started || !isConnectError(err) {
			return zero, err
		}
		next := f.nextAlive()
		if next < 0 {
			return zero, err
		}
		f.switchTo(next, "连接失败："+err.Error())
	}
	return zero, fmt.Errorf("llm-failover: 所有候选端点均连接失败")
}

// Stream 实现 llm.Provider（连接类失败自动换端点重放整个请求）。
func (f *endpointFailover) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		res, err := streamWithFailover(f, func(prov llm.Provider) (iter.Seq2[llm.StreamEvent, error], bool, error) {
			// started 恒 false：连接类失败只可能发生在首个事件前（一旦连上，
			// 中途断连的错误串不命中连接特征，isConnectError 会透传不切换）。
			return prov.Stream(ctx, req), false, nil
		})
		if err != nil {
			var zero llm.StreamEvent
			yield(zero, err)
			return
		}
		for ev, err := range res {
			if !yield(ev, err) {
				return
			}
		}
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
