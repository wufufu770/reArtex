package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/netguard"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) listActiveFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	items, err := pg.ListActiveFindingRetests(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) listFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	items, err := pg.ListFindingRetests(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) startFindingRetest(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var req struct {
		Notes string `json:"notes"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if utf8.RuneCountInString(req.Notes) > 4000 {
		writeErr(w, 400, "复测补充说明最多 4000 个字符")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	a, err := pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a == nil || !a.Enabled {
		writeErr(w, 409, "漏洞复测 Agent 不存在或未启用，请在 Agent 管理中配置 retester")
		return
	}
	for _, key := range []string{"get_finding_retest_context", "record_finding_retest_result"} {
		t, err := pg.GetTool(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if t == nil || !t.Enabled || !slices.Contains(t.Agents, a.Key) {
			writeErr(w, 409, "请为复测 Agent 启用并绑定工具："+key)
			return
		}
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		writeErr(w, 503, s.chatUnavailableReason())
		return
	}
	if s.ctx.Err() != nil {
		writeErr(w, 503, "服务正在停止")
		return
	}
	retest, conv, created, err := pg.CreateFindingRetest(r.Context(), id, req.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "finding not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if created {
		busyKey := s.convBusyKey(conv.ID)
		s.chatMu.Lock()
		s.chatBusy[busyKey] = true
		s.chatMu.Unlock()
		// T8（方案 v10 §6 三期）：复测 agent 开跑前，异步做一次 baseline 探测
		// （proof 请求去掉攻击载荷），结论落 effect_pre_existed——标注不是否决，
		// 不阻塞复测，探测失败保持 NULL。
		s.runFindingRetestBaseline(retest)
		s.runConversation(conv, retest.InitialMessage(), busyKey)
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"retest": retest, "created": created})
}

// Both tools derive the finding from server-owned conversation context. Tool
// arguments cannot redirect a result into a different finding or conversation.
func (s *Server) findingRetestTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("get_finding_retest_context", "读取当前复测会话关联的漏洞证据快照、复测状态、补充说明与当前任务约束。无参数，只能读取本会话。",
			objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
				r, err := s.m.pg.FindingRetestForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if r == nil {
					return actool.Errorf("当前会话未关联复测记录，请从漏洞详情发起复测"), nil
				}
				var constraints []db.Constraint
				f, err := s.m.pg.GetFinding(r.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f != nil && f.TaskID != nil {
					if task, ok := s.m.Task(strconv.FormatInt(*f.TaskID, 10)); ok {
						constraints, err = task.Store.ListConstraints()
						if err != nil {
							return actool.Errorf(err.Error()), nil
						}
					}
				}
				return jsonResult(map[string]any{"retest": r, "current_constraints": constraints})
			}),
		wrTool("record_finding_retest_result", "为当前复测会话保存唯一结论；原漏洞证据与报告保持不变。会话成功结束且结论为 fixed 时，系统自动将漏洞状态改为已修复；其他结论保留原状态。必须提供本次实际检查的证据，无法确认时写明阻塞原因。",
			objSchema(map[string]any{
				"verdict":  map[string]any{"type": "string", "enum": []string{"reproduced", "fixed", "inconclusive"}},
				"summary":  strParam("本次复测结论摘要"),
				"evidence": strParam("Markdown：本次实际步骤、观察、对照、结论依据；无法确认则列出已检查内容和阻塞原因"),
			}, "verdict", "summary", "evidence"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					Verdict  string `json:"verdict"`
					Summary  string `json:"summary"`
					Evidence string `json:"evidence"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err := s.m.pg.RecordFindingRetestResult(ctx, intercept.ConvIDFromContext(ctx), a.Verdict, a.Summary, a.Evidence); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(map[string]any{"saved": true, "verdict": a.Verdict})
			}),
	}
}

// Seed the editable agent atomically, without an automatic discovery trigger.
// Once seeded, user edits/deletion survive restarts; a pre-existing key is kept.
func (s *Server) seedFindingRetester() error {
	for _, t := range s.findingRetestTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings, _ := json.Marshal([]string{db.FindingRetestAgentKey})
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, bindings); err != nil {
			return err
		}
	}
	const flag = "finding_retester_seed_v1"
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock this migration, including concurrent server initialization.
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741010)`); err != nil {
		return err
	}
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, flag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled)
	VALUES ($1,'漏洞复测','从漏洞详情手动启动，读取原证据并保存独立复测结论。','assistant',false,true)
	ON CONFLICT (key) DO NOTHING RETURNING id`, db.FindingRetestAgentKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
		VALUES ($1,1,$2,'内置默认','system') RETURNING id`, id, agent.RetesterDefaultPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) finishRetest(id int64, status, reason string) {
	if err := s.m.pg.FinishFindingRetest(id, status, reason); err != nil {
		// Surface failure to the conversation runner rather than inventing a result.
		log.Printf("[retest %d] finish: %v", id, err)
	}
}

// ---------- T8（方案 v10 §6 三期）：复测 baseline 探测 ----------
//
// 复测 agent 开跑前，按 finding 的 proof 证据「复制一份但去掉攻击载荷」打一次
// 目标，把「效果是否本来就存在」记成 effect_pre_existed（TRUE/FALSE/NULL）。
// 这是机械分层（确定性代码），不是复测结论：标注不改判三态 verdict，也不参与
// finished 判定——它只给下游（报告/UI）一个「这个效果可能是环境自带的」的
// 观测位。探测失败/无证据时列保持 NULL（=未探测/不可判），复测照常进行。

// retestBaselineBodyLimit 限制 baseline 响应体读取量（探测只做特征比对）。
const retestBaselineBodyLimit = 256 << 10

// retestBaselineTimeout 是单次 baseline 请求的预算（含走代理的握手）。
const retestBaselineTimeout = 12 * time.Second

// runFindingRetestBaseline 异步执行 baseline 探测并落库。best-effort：任何失败
// 只留日志，不阻塞复测会话，不发明结论。
func (s *Server) runFindingRetestBaseline(r *db.FindingRetest) {
	if s == nil || s.m == nil || s.m.pg == nil || r == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, retestBaselineTimeout+5*time.Second)
		defer cancel()
		effect, err := s.probeFindingBaseline(ctx, r.FindingID)
		if err != nil {
			log.Printf("[retest %d] baseline 探测未执行（%v）——effect_pre_existed 保持 NULL", r.ID, err)
		}
		if err := s.m.pg.SetFindingRetestBaseline(ctx, r.ID, effect); err != nil {
			log.Printf("[retest %d] baseline 探测结论落库失败: %v", r.ID, err)
			return
		}
		if effect != nil {
			log.Printf("[retest %d] baseline 探测完成：effect_pre_existed=%v", r.ID, *effect)
		}
	}()
}

// probeFindingBaseline 的机械口径：
//   - 目标 = proof 快照 URL 去掉 query（GET 干净页面）——攻击载荷的主要落点是
//     query/body；快照只存请求头不存 body，落在 body 的载荷取不到 ⇒ 此时返回
//     「不可判」（NULL），不猜；
//   - 载荷标记 = 原 URL query 的解码 value（len ≥ 4，去重，最多 8 个）；
//   - 判定 = baseline 响应体里是否出现任一载荷标记：载荷没打、效果仍在
//     （页面原样回显了载荷形态的内容）⇒ effect 本来就存在（TRUE）；
//   - 出网带 finding 所属任务的身份走录制代理（范围判定照常生效，留痕走
//     流量录制）；代理不可用时直连 + netguard 守卫兜底。
func (s *Server) probeFindingBaseline(ctx context.Context, findingID int64) (*bool, error) {
	ft, err := s.m.pg.GetFindingTraffic(ctx, findingID)
	if err != nil {
		return nil, err
	}
	var proof *db.TrafficEvidenceSnapshot
	if ft != nil {
		for i := range ft.Bindings {
			if ft.Bindings[i].Role == "proof" {
				snap := ft.Bindings[i].Snapshot
				proof = &snap
				break
			}
		}
	}
	if proof == nil || proof.URL == "" {
		return nil, fmt.Errorf("finding %d 没有 proof 证据，无法构造 baseline 请求", findingID)
	}
	u, err := url.Parse(proof.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("proof URL 不可解析: %q", proof.URL)
	}
	tokens := baselinePayloadTokens(u)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("proof 请求的载荷不在 URL query（body 载荷不在快照内），不可判")
	}
	bare := *u
	bare.RawQuery = ""
	bare.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bare.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range baselineAuthHeaders(proof.ReqHead) {
		req.Header.Set(k, v)
	}
	taskID := int64(0)
	if f, ferr := s.m.pg.GetFinding(findingID); ferr == nil && f != nil && f.TaskID != nil {
		taskID = *f.TaskID
	}
	client := s.retestBaselineClient(taskID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("baseline 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, retestBaselineBodyLimit))
	if err != nil && len(body) == 0 {
		return nil, fmt.Errorf("baseline 响应读取失败: %w", err)
	}
	effect := false
	for _, tok := range tokens {
		if strings.Contains(strings.ToLower(string(body)), tok) {
			effect = true
			break
		}
	}
	return &effect, nil
}

// baselinePayloadTokens 提取 URL query 的载荷标记：解码后的 value（len ≥ 4，
// 去重，最多 8 个，小写化供包含比较）。value 太短的（如分页参数 page=2）不进
// 标记集——它们本来就会出现在任何页面上，会把判定打歪。
func baselinePayloadTokens(u *url.URL) []string {
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	tokens := make([]string, 0, 4)
	for _, values := range q {
		for _, v := range values {
			v = strings.ToLower(strings.TrimSpace(v))
			if len([]rune(v)) < 4 || seen[v] {
				continue
			}
			seen[v] = true
			tokens = append(tokens, v)
			if len(tokens) >= 8 {
				return tokens
			}
		}
	}
	return tokens
}

// baselineAuthHeaders 从 proof 快照的请求头块里抄认证头（Cookie/Authorization）
// ——baseline 要看「登录态下页面本来长什么样」。请求头块形如
// "GET /path?q=1 HTTP/1.1\nHeader: value\n..."（见 traffic.recordExchange）。
func baselineAuthHeaders(reqHead string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(reqHead, "\n") {
		line = strings.TrimRight(line, "\r")
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "cookie" || k == "authorization" {
			if v = strings.TrimSpace(v); v != "" {
				out[k] = v
			}
		}
	}
	return out
}

// retestBaselineClient 造 baseline 探测的 HTTP 客户端：优先带任务身份走录制
// 代理（范围判定生效；与 enrich.buildClient 同一取舍——MITM 代理重签证书、
// 靶标常是自签 ⇒ 跳过 TLS 校验，这是渗透探测不是保密信道）；代理不可用时
// 直连 + netguard 守卫。不跟随重定向（第一跳响应就够判定；跟随会把探测面
// 扩大到目标之外）。
func (s *Server) retestBaselineClient(taskID int64) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 8 * time.Second,
	}
	if proxyAddr := s.m.ProxyAddr(); proxyAddr != "" {
		if taskID > 0 {
			proxyAddr = withEgressTaskUserinfo(proxyAddr, taskID)
		}
		if pu, perr := url.Parse(proxyAddr); perr == nil && (pu.Scheme == "http" || pu.Scheme == "https") {
			tr.Proxy = http.ProxyURL(pu)
		}
	} else {
		// 无代理（捕获关 + 无全局代理）：直连仍过 netguard 段守卫（守卫在
		// DialContext 的拨号 Control 里，借它的 DialContext 即可）。
		guarded := netguard.AgentEgress.Transport(nil)
		tr.DialContext = guarded.DialContext
	}
	return &http.Client{
		Timeout:   retestBaselineTimeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// withEgressTaskUserinfo 把任务身份令牌放进代理 URL 的 userinfo（与 agent 包
// 的 egressProxyURL 同一约定：Go 会把它变成 Proxy-Authorization Basic，
// traffic.parseProxyAuthTask 认这个形态）。非 http(s) 代理原样返回。
func withEgressTaskUserinfo(proxyAddr string, taskID int64) string {
	u, err := url.Parse(proxyAddr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return proxyAddr
	}
	u.User = url.User(traffic.SignEgressTaskToken(taskID))
	return u.String()
}
