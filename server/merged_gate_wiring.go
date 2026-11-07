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

// wireMergedGateDeps 把合并闸依赖补进 ChainDeps（装配点在 setupSoDChain）。
func (s *Server) wireMergedGateDeps(deps *sod.ChainDeps) {
	deps.MergedGateMode = s.mergedGateMode
	deps.IntelSource = s.intelSourceAllowed
	deps.Categories = s.sodCategoriesSnapshot
	deps.Adjudicator = s.sodAuditorAdjudicator
	deps.ElevateAsk = s.elevateAsk
	deps.CategoryUsed = s.sodCategoryUsed
	deps.RecordShadowDiff = s.recordSodShadowDiff
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

// ensureAuditorAgent 播种 auditor agent 行（幂等）+ 内置提示词；没有 agent 行
// 就没有 CurrentPrompt 可读，内置 auditorPrompt 仍是兜底，这里补齐身份。
func (s *Server) ensureAuditorAgent() {
	if _, err := s.m.pg.GetAgentByKey(sodpolicy.AuditorAgentKey); err == nil {
		return
	}
	_, err := s.m.pg.Exec(`
		INSERT INTO agents (key, name, role, builtin, enabled, web_search, interactive_shell)
		VALUES ($1, $2, 'assistant', true, false, false, false)
		ON CONFLICT (key) DO NOTHING`,
		sodpolicy.AuditorAgentKey, "审计裁决")
	if err != nil {
		log.Printf("[auditor] agent 行播种失败（裁决回落内置提示词）: %v", err)
	}
}

var _ = context.Background
var _ = sync.Mutex{}
