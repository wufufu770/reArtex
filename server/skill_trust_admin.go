package server

// 本文件是 A-02（0.15.91）skill 信任机制的 operator 侧收口面：
//
//  1. POST /api/skills/{name}/trust —— 唯一能把 skill 档位升回 trusted 的显式入口
//     （树哈希失配降级后，agent 没有任何路径能自己恢复；operator 复核内容后在此授信，
//     操作进审计链）。
//  2. reconcileSkillTrust —— 启动期信任记录对账：存量单文件哈希记录静默迁移为
//     树哈希（档位原样保留，列表页展示前后一致、无静默翻转）；无记录的 skill 补
//     标记 trusted（升级前「无记录 = 随镜像分发 = operator 布置」按 trusted 展示；
//     补钉之后任何记录之外的改动都会失配降级——迁移同时把树钉住，是加固不是放水）。
//  3. create_skill 批量创建的速率告警：滑动 1 小时窗口计数，超阈值（settings 可配，
//     默认 10 次/小时）记审计事件 + 日志——批量自撰 skill 是 prompt-injection 面
//     的量变信号，值得一条显式留痕。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// skillTrustThresholdSetting 是批量创建告警阈值的热改键（settings 表）。
// 非法值（非数字/≤0）一律回落默认——配置错误不能变成「关掉告警」。
const skillTrustThresholdSetting = "skill_trust.create_alert_threshold"

const skillTrustDefaultThreshold = 10

// fsSkillTrust 处理 POST /api/skills/{name}/trust。
//
// 仅授权 JWT 可达（走全局 requireAuth）；body: {"tier":"trusted"|"untrusted",
// "reason":"..."}。先重算树哈希落记录（operator 身份），再写审计链——审计的是
// 「operator 在看过内容之后做的显式裁决」，tier 与理由都要在链上可查。
func (s *Server) fsSkillTrust(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	var body struct {
		Tier   string `json:"tier"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体必须是 JSON: "+err.Error())
		return
	}
	if body.Tier != skillTierTrusted && body.Tier != skillTierUntrusted {
		writeErr(w, 400, `tier 必须是 "trusted" 或 "untrusted"`)
		return
	}
	if _, err := skillTreeHash(s.skillDir, name); err != nil {
		writeErr(w, 404, "skill 不存在或目录为空："+err.Error())
		return
	}
	recordSkillTrust(pg, s.skillDir, name, body.Tier, skillTrustWriterOperator)
	_, _ = pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "operator",
		ToolName: "skills",
		Action:   "skill.trust",
		Source:   "operator",
		Reason:   fmt.Sprintf("skill %s 由 operator 显式标记为 %s（A-02 授信端点）", name, body.Tier),
		Payload:  []byte(fmt.Sprintf(`{"skill":%q,"tier":%q,"reason":%q}`, name, body.Tier, body.Reason)),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "skill": name, "tier": body.Tier})
}

// reconcileSkillTrust 是升级/启动期的一次性信任记录对账（A-02 存量迁移）。
//
// 三种情况：
//   - 无记录 ⇒ 补 treeHash|trusted：升级前「无记录 = 随镜像分发 = operator 布置」
//     按 trusted 展示，补钉保持展示一致（无静默翻转）；树同时被钉住，此后的
//     记录之外改动照常失配降级；
//   - 存量单文件哈希记录（值恰好等于 sha256(SKILL.md)）⇒ 换算成树哈希，档位
//     原样保留——格式迁移，不是重新裁决；
//   - 其它（已是树哈希记录 / 未知格式）⇒ 不动：失配交给判定路径降级 + 审计，
//     reconcile 不掩盖任何已经发生的改动。
//
// 幂等：迁移后记录不再是单文件哈希，重复启动零开销。
func reconcileSkillTrust(pg *db.DB, skillDir string) {
	if pg == nil || skillDir == "" {
		return
	}
	entries, err := os.ReadDir(skillDir)
	if err != nil {
		return
	}
	migrated, pinned := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !validSkillName(name) {
			continue
		}
		rec, ok, err := pg.GetSetting(skillTrustKeyPrefix + name)
		if err != nil {
			log.Printf("[skills] 信任对账读取 %s 失败: %v", name, err)
			continue
		}
		if !ok || rec == "" {
			tree, err := skillTreeHash(skillDir, name)
			if err != nil {
				continue // 空目录/读不了：留给判定路径 fail-closed
			}
			_ = pg.SetSetting(skillTrustKeyPrefix+name, tree+"|"+skillTierTrusted)
			pinned++
			continue
		}
		sum, tier, found := strings.Cut(rec, "|")
		if !found {
			continue
		}
		legacy, err := skillLegacyContentHash(skillDir, name)
		if err != nil || sum != legacy {
			continue
		}
		tree, err := skillTreeHash(skillDir, name)
		if err != nil {
			continue
		}
		_ = pg.SetSetting(skillTrustKeyPrefix+name, tree+"|"+tier)
		migrated++
	}
	if pinned > 0 || migrated > 0 {
		log.Printf("[skills] A-02 信任记录对账完成：单文件哈希记录迁移 %d 条，无记录 skill 补钉 %d 个", migrated, pinned)
	}
}

// skillCreateRateWindow 是 create_skill 批量创建的滑动窗口状态（进程内；
// 重启归零是该口径的已知边界——与调度面计数器一致，看趋势看时间序列）。
var skillCreateRate struct {
	mu      sync.Mutex
	times   []time.Time
	alerted bool // 本窗口已告过警：同一批超量创建只记一条审计，不按次数刷链
}

// skillCreateAlertThreshold 读热改阈值；非法值回落默认并在日志里说明。
func skillCreateAlertThreshold(pg *db.DB) int {
	if pg == nil {
		return skillTrustDefaultThreshold
	}
	v, ok, err := pg.GetSetting(skillTrustThresholdSetting)
	if err != nil || !ok || v == "" {
		return skillTrustDefaultThreshold
	}
	n, perr := strconv.Atoi(v)
	if perr != nil || n <= 0 {
		log.Printf("[skills] %s=%q 不是正整数，回落默认 %d", skillTrustThresholdSetting, v, skillTrustDefaultThreshold)
		return skillTrustDefaultThreshold
	}
	return n
}

// recordSkillCreateAndAlert 在 create_skill 成功后记账并判定是否超速。
//
// 超阈值时第一次记审计事件（actor=platform，批量自撰 skill 是 prompt-injection
// 面的量变信号）并打告警日志；窗口内后续创建不再重复记链，窗口滑出超量区间后
// 重新武装。函数在工具调用路径上同步执行：窗口最多保留阈值相关条数，开销可忽略。
func recordSkillCreateAndAlert(pg *db.DB, name string) {
	if pg == nil {
		return
	}
	now := time.Now()
	window := time.Hour
	threshold := skillCreateAlertThreshold(pg)

	shouldAlert := false
	skillCreateRate.mu.Lock()
	kept := skillCreateRate.times[:0]
	for _, ts := range skillCreateRate.times {
		if now.Sub(ts) < window {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	skillCreateRate.times = kept
	over := len(kept) > threshold
	if over && !skillCreateRate.alerted {
		skillCreateRate.alerted = true
		shouldAlert = true
	}
	if !over {
		skillCreateRate.alerted = false // 滑回阈值内：重新武装
	}
	count := len(kept)
	skillCreateRate.mu.Unlock()

	if !shouldAlert {
		return
	}
	log.Printf("[skills][alert] skill 批量创建超速：1 小时内已创建 %d 个（阈值 %d），最近创建 %s", count, threshold, name)
	_, _ = pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "platform",
		ToolName: "create_skill",
		Action:   "skill.create_rate_alert",
		Source:   "platform",
		Reason:   fmt.Sprintf("skill 批量创建超速：1 小时内 %d 个（阈值 %d），最近创建 %s——批量自撰内容默认 untrusted，请复核", count, threshold, name),
		Payload:  []byte(fmt.Sprintf(`{"count":%d,"threshold":%d,"last":%q}`, count, threshold, name)),
	})
}

// resetSkillCreateRate 供测试重置窗口状态。
func resetSkillCreateRate() {
	skillCreateRate.mu.Lock()
	skillCreateRate.times = nil
	skillCreateRate.alerted = false
	skillCreateRate.mu.Unlock()
}
