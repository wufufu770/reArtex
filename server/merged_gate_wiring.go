package server

// ---------- 合并闸装配（方案 v4.1：ChainDeps 的宿主侧依赖）----------
//
// 三态开关 / 情报源白名单 / 种类表快照 / 影子对照落库 / 人工 ask 通路 /
// cap 计数——全部集中在这一个文件，ChainDeps 的接线只做字段赋值。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/internal/sod"
	"github.com/Autumn-27/artex/internal/sodpolicy"
)

const settingIntelSources = "sod.intel_sources"

// mergedGateMode 读三态开关（默认 off——部署后由操作者显式推进）。
func (s *Server) mergedGateMode() string {
	v, _, _ := s.m.pg.GetSetting(sodpolicy.SettingMergedGate)
	switch strings.TrimSpace(v) {
	case sodpolicy.MergedGateShadow:
		return sodpolicy.MergedGateShadow
	case sodpolicy.MergedGateEnforce:
		return sodpolicy.MergedGateEnforce
	default:
		return sodpolicy.MergedGateOff
	}
}

// intelSourceList 读情报源白名单（settings JSON 数组；未配置回落默认 17 源），
// 10s 缓存（双消费之一；另一处在 traffic/egress.go）。
func (s *Server) intelSourceList() []string {
	s.intelMu.Lock()
	defer s.intelMu.Unlock()
	if s.intelCache != nil && time.Since(s.intelAt) < 10*time.Second {
		return s.intelCache
	}
	list := []string(sodpolicy.DefaultIntelSources)
	if raw, ok, _ := s.m.pg.GetSetting(settingIntelSources); ok && strings.TrimSpace(raw) != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && len(parsed) > 0 {
			list = parsed
		}
	}
	s.intelCache = list
	s.intelAt = time.Now()
	return list
}

// intelSourceAllowed 是 ChainDeps.IntelSource 的落点。
func (s *Server) intelSourceAllowed(host string) bool {
	return sodpolicy.IntelSourceAllowed(host, s.intelSourceList())
}

// sodCategoriesSnapshot 返回种类表快照（version 号失配才重查 DB；
// 版本号由每个 CRUD bump——热改秒级生效，热路径零查询）。
func (s *Server) sodCategoriesSnapshot() ([]sodpolicy.SodCategory, uint64) {
	pg := s.m.pg
	verStr := pg.SodCategoriesVersion()
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if s.catCache != nil && s.catVer == verStr {
		return s.catCache, s.catVerNum
	}
	rows, err := pg.ListSodCategories()
	if err != nil {
		// 查询失败用旧快照顶住（可用性优先于新鲜度；种类表是放宽面，
		// 旧快照最坏多拦/多放一个已改行的窗口期，且审计有 version 可查）。
		if s.catCache != nil {
			return s.catCache, s.catVerNum
		}
		return nil, 0
	}
	out := make([]sodpolicy.SodCategory, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SodCategory)
	}
	s.catCache = out
	s.catVer = verStr
	s.catVerNum++
	return s.catCache, s.catVerNum
}

// recordSodShadowDiff 是 ChainDeps.RecordShadowDiff 的落点（best-effort）。
func (s *Server) recordSodShadowDiff(diff sod.ShadowDiff) {
	row := sodShadowRowOf(diff)
	if err := s.m.pg.RecordSodShadowDiff(row); err != nil {
		log.Printf("[sod-shadow] 对照落库失败（忽略）: %v", err)
	}
}

// elevateAsk 是 ChainDeps.ElevateAsk 的落点：把合并闸的升级请求转成一次
// 拦截层 ask（审批页 + 群聊卡共用 HandleAsk 通路），阻塞到人工裁决或超时。
// 返回 (批准?, 批准人, err)。
func (s *Server) elevateAsk(ctx context.Context, req sod.AskRequest) (bool, string, error) {
	if s.m == nil || s.m.interceptor == nil {
		return false, "", fmt.Errorf("拦截器未装配")
	}
	dec := intercept.Decision{
		Action:         "ask",
		Message:        req.Reason,
		RuleName:       "[合并闸] auditor/种类升级",
		TimeoutEnabled: true,
		TimeoutSeconds: askRuleDefaultTimeoutSeconds,
		TimeoutAction:  "deny",
	}
	convID := req.ConversationID
	approved := s.m.interceptor.HandleAsk(ctx, convID, dec, req.ToolName, []byte(req.PayloadPreview))
	if !approved {
		return false, "", nil
	}
	return true, "human", nil
}

// sodCategoryUsed 是 ChainDeps.CategoryUsed 的落点（账本即计数器）。
func (s *Server) sodCategoryUsed(ctx context.Context, taskID int64, category string) (int, error) {
	return s.m.pg.CountSodCategoryUsed(taskID, category)
}

// baselineDestructiveStopper 是 ChainDeps.OnBaselineDestructive 的落点
// （方案 v6 C1，用户拍板「超出界限外的删除动作就停」）：锁定基线删除/破坏
// 命中 ⇒ 自动暂停任务 + 引擎活动 + 绑定会话播报；人工在任务页续跑。
func (s *Server) baselineDestructiveStopper(taskID int64, toolName, hit string) {
	if s.m == nil || taskID <= 0 {
		return
	}
	numeric := taskID
	t, ok := s.m.Task(fmt.Sprintf("%d", taskID))
	if !ok || t == nil {
		return
	}
	// 暂停（幂等：已暂停/终态的任务 Pause 内部自然无效）。
	if _, aerr := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByUser); aerr != nil {
		log.Printf("[baseline] task %d 自动暂停失败（拦截仍生效）: %v", taskID, aerr)
	}
	summary := fmt.Sprintf("【已自动暂停】任务 #%d 出现删除/破坏类动作（%s：%s）——动作已拦截，任务已暂停待人工确认后续跑", taskID, toolName, hit)
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true, Summary: summary})
	if sess, serr := s.m.pg.SessionByTask(numeric); serr == nil && sess != nil {
		s.say(sess.ID, numeric, "system", "system", summary)
	}
	log.Printf("[baseline] task %d 删除/破坏命中，已自动暂停：tool=%s hit=%s", taskID, toolName, hit)
}

// capHitSupervisor 是 ChainDeps.OnCapHit 的落点（方案 v9 批次1⑥）：cap 命中
// ⇒ 事件直触一次监督轮（30s/任务节流防风暴），auto 可即时 sod_adjust_cap。
func (s *Server) capHitSupervisor(taskID int64, category string) {
	if s.m == nil || taskID <= 0 {
		return
	}
	headline := fmt.Sprintf("【cap 事件】拦截种类「%s」本任务配额触发（事件直触，非轮询）。请结合快照判断该 cap 是否误伤：误伤即用 sod_adjust_cap 放宽；合理则维持并考虑给剩余头部续派。", category)
	s.fireSupervisionEvent(fmt.Sprintf("%d", taskID), headline)
}

// wireMergedGateDeps 把合并闸依赖补进 ChainDeps（装配点在 setupSoDChain）。
func (s *Server) wireMergedGateDeps(deps *sod.ChainDeps) {
	deps.MergedGateMode = s.mergedGateMode
	deps.IntelSource = s.intelSourceAllowed
	deps.Categories = s.sodCategoriesSnapshot
	deps.Adjudicator = s.sodAuditorAdjudicator
	deps.ElevateAsk = s.elevateAsk
	deps.CategoryUsed = s.sodCategoryUsed
	deps.RecordShadowDiff = s.recordSodShadowDiff
	deps.OnCapHit = s.capHitSupervisor
	deps.OnBaselineDestructive = s.baselineDestructiveStopper
}

// sodShadowRowOf 转换 internal/sod.ShadowDiff → db 行（db 不 import internal）。
func sodShadowRowOf(d sod.ShadowDiff) struct {
	TaskID        int64
	ToolName      string
	PayloadDigest string
	OldVerdict    string
	OldReason     string
	NewVerdict    string
	NewReason     string
} {
	return struct {
		TaskID        int64
		ToolName      string
		PayloadDigest string
		OldVerdict    string
		OldReason     string
		NewVerdict    string
		NewReason     string
	}{d.TaskID, d.ToolName, d.PayloadDigest, d.OldVerdict, d.OldReason, d.NewVerdict, d.NewReason}
}

// ensureAuditorAgent 播种 auditor agent 行（幂等、enabled=true——v6 B3：它是
// 合并闸的常驻裁决岗，必须在 agent 页可见可配置）+ 提示词版本（DB 优先，
// 内置兜底）。
func (s *Server) ensureAuditorAgent() {
	var ag *db.Agent
	ag, err := s.m.pg.GetAgentByKey(sodpolicy.AuditorAgentKey)
	if err != nil || ag == nil {
		if _, ierr := s.m.pg.Exec(`
			INSERT INTO agents (key, name, role, builtin, enabled, web_search, interactive_shell)
			VALUES ($1, $2, 'assistant', true, true, false, false)
			ON CONFLICT (key) DO UPDATE SET enabled=true, name=EXCLUDED.name`,
			sodpolicy.AuditorAgentKey, "审计裁决"); ierr != nil {
			log.Printf("[auditor] agent 行播种失败（裁决回落内置提示词）: %v", ierr)
			return
		}
		if ag, err = s.m.pg.GetAgentByKey(sodpolicy.AuditorAgentKey); err != nil || ag == nil {
			return
		}
	}
	// enabled 兜底（v6：历史行可能停在 false——裁决岗不该禁用）。
	if !ag.Enabled {
		_, _ = s.m.pg.Exec(`UPDATE agents SET enabled=true WHERE key=$1`, sodpolicy.AuditorAgentKey)
	}
	// 提示词版本：无任何版本时把内置 prompt 落 agent_prompts（agent 页可编辑）。
	if body, err := s.m.pg.CurrentPrompt(ag.ID); err == nil && strings.TrimSpace(body) == "" {
		if _, perr := s.m.pg.SavePrompt(ag.ID, auditorPrompt, "v6 裁决口径（exploit_relevance）", "system"); perr != nil {
			log.Printf("[auditor] 提示词播种失败（用内置兜底）: %v", perr)
		}
	}
}

var _ = context.Background
var _ = sync.Mutex{}
