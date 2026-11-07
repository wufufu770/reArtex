package sod

// ---------- 拦截种类表匹配引擎（方案 v4.1 §2/§3）----------
//
// 种类表是用户可编辑的「该拦什么」配置；内建种类以确定性分类器为识别器，
// custom 种类是用户正则。匹配按 priority DESC 取第一条命中（与拦截规则同一
// 惯例）。快路径就是表里 action=pass 的行——单一裁决面，用户改动作即全链
// 生效（v4.1 §10-F3 修订）。
//
// 匹配器必须廉价：内建 = Classify 一次（µs 级）+ 谓词；custom = 正则
// （编译结果按 pattern 缓存，process 级）。

import (
	"regexp"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/internal/sodpolicy"
)

// ---------- 合并闸的跨单元类型（server 装配、chain 消费）----------

// AuditorRequest 是链交给 auditor 的一次裁决请求。
type AuditorRequest struct {
	TaskID         int64
	ConversationID int64
	ToolName       string
	SourceAgent    string
	// PayloadPreview 是脱敏后的载荷预览（RedactPreview 口径）。
	PayloadPreview string
	// PayloadDigest 绑定裁决缓存键（同字节重试不重复问 LLM）。
	PayloadDigest string
	ScopeDigest   string
	// Reason 是请求语境：命中的种类名 / out_of_scope / unclear。
	Reason string
}

// AuditorVerdict 是 auditor 的裁决。
type AuditorVerdict struct {
	Verdict       string // pass | deny | ask
	ReasonCode    string
	Reason        string
	Category      string
	TargetHint    string
	AnomalyFlag   bool
	AnomalyReason string
	DecidedBy     string // "auditor" / "cache"
	LatencyMs     int
}

// AskRequest 是升级人工的一次请求。
type AskRequest struct {
	TaskID         int64
	ConversationID int64
	ToolName       string
	Reason         string
	PayloadPreview string
}

// ShadowDiff 是影子模式的一行对照（旧链 vs 新链，只记差异不影响动作）。
type ShadowDiff struct {
	TaskID        int64
	ToolName      string
	PayloadDigest string
	OldVerdict    string
	OldReason     string
	NewVerdict    string
	NewReason     string
}

// ---------- 匹配引擎 ----------

var matcherMu sync.Mutex
var matcherCache = map[string]*regexp.Regexp{}

func compiledPattern(pattern string) *regexp.Regexp {
	matcherMu.Lock()
	defer matcherMu.Unlock()
	if re, ok := matcherCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		matcherCache[pattern] = nil // 坏正则缓存为 nil：永远不命中（不 panic 不放行）
		return nil
	}
	matcherCache[pattern] = re
	return re
}

// MatchCategory 按 priority DESC 取第一条命中；未命中返回 nil。
// cls 必须已由 Classify 产出（调用方保证基线预检先行——锁定行到这里是不可达的，
// 保留匹配仅为表语义完整）。
func MatchCategory(cats []sodpolicy.SodCategory, tool string, cls ClassResult, payload []byte) *sodpolicy.SodCategory {
	var best *sodpolicy.SodCategory
	for i := range cats {
		cat := &cats[i]
		if !cat.Enabled {
			continue
		}
		if best != nil && cat.Priority <= best.Priority {
			continue
		}
		if !categoryMatches(cat, tool, cls, payload) {
			continue
		}
		best = cat
	}
	return best
}

// categoryMatches 判定单条种类是否命中。
func categoryMatches(cat *sodpolicy.SodCategory, tool string, cls ClassResult, payload []byte) bool {
	if cat.Kind == "custom" {
		re := compiledPattern(cat.Pattern)
		return re != nil && re.Match(payload)
	}
	switch cat.Pattern {
	case sodpolicy.RecognizeDestructive, sodpolicy.RecognizePlatformCfg:
		// 锁定基线在链与 guard 预检先行，到这里不可达；防御性返回 false。
		return false
	case sodpolicy.RecognizeTaskDirLocal:
		return cls.Kind == ClassLocalSafe
	case sodpolicy.RecognizeIntelSource:
		return cls.Kind == ClassIntelSource
	case sodpolicy.RecognizeDNSWhois:
		return cls.DNSQuery
	case sodpolicy.RecognizeBenignNoop:
		return cls.Kind == ClassBenign
	case sodpolicy.RecognizeOutsideWrite:
		return cls.Kind == ClassOutsideWrite
	case sodpolicy.RecognizeSensitiveRead:
		return sodpolicy.SensitiveHit(payload) != ""
	case sodpolicy.RecognizeStateChange:
		return cls.Kind == ClassStateChange
	case sodpolicy.RecognizeScopeExpansion:
		return cls.Kind == ClassScopeExpansion
	case sodpolicy.RecognizeEgressNewHost:
		// 「有远端目标」的种类语义；在段内的调用会被闸门正常放行，真正落到
		// auditor 的只有出段者（链在 scope 判定后才路由，见 chain）。
		return cls.Kind == ClassTargetKnown
	case sodpolicy.RecognizeMCPCall:
		return strings.HasPrefix(strings.ToLower(tool), "mcp__")
	case sodpolicy.RecognizeAssetInsert:
		return strings.EqualFold(tool, "insert_assets")
	case sodpolicy.RecognizeFindingWrite:
		return strings.EqualFold(tool, "report_finding")
	}
	return false
}
