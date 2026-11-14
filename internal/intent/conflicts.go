// Package intent 实现意图间冲突的确定性检测（0.14.1 批次二 B1）。
//
// 设计纪律（施工方案 v2，用户裁定）：
//   - 检测是确定性规则（代码常量，带稳定 ID）；裁决优先规则 tie-breaker，
//     规则裁不了才交 arbiter agent；人不参与常规仲裁（只兜底熔断）。
//   - 任务给的资产范围是权威基线：意图锚点写入时已 fail-closed 保证资产在
//     scope 内，检测器不重复判范围；无资产锚的意图结构化不足，不参与检测
//     （宁漏勿误——误报仲裁比漏报更伤）。
//   - 只对结构化可判的冲突检测：payload 缺预测字段的意图对不产冲突。
package intent

import (
	"sort"
	"strings"
)

// Signal 是参与冲突检测的意图信号（server 侧从节点 payload + 锚资产提取）。
type Signal struct {
	NodeID    int64    // exploration_nodes.id（kind='intent'）
	Kind      string   // verify | recon | exploit
	Summary   string   // 摘要（对偶词检测用）
	VulnClass string   // predicted_vulnclass（空=结构化不足）
	Actions   []string // predicted_actions
	AssetIDs  []int64  // 锚定资产（去重升序；空=不参与检测）
}

// Found 是一次命中的冲突（含规则裁决结论——winner 为空表示交 arbiter）。
type Found struct {
	RuleID    string         `json:"rule_id"`
	Basis     map[string]any `json:"basis"`
	Winner    string         `json:"winner,omitempty"`    // "a"|"b"|""（空=交 arbiter）
	Reason    string         `json:"reason,omitempty"`    // tie-break 理由（写审计）
	SideA     Signal         `json:"-"`
	SideB     Signal         `json:"-"`
}

// Rule 是一条冲突检测规则（代码常量注册表；ID 落 intent_conflicts.detect_rule，
// 二期数据驱动时 schema 零迁移）。
type Rule struct {
	ID       string
	Detect   func(a, b Signal) (map[string]any, bool)
	TieBreak func(a, b Signal) (winner, reason string) // "" = 交 arbiter
}

// actionClasse 把动作清单归成行为类（结构化判定唯一依据；大小写不敏感）。
//   - mutating：状态改变类（利用/爆破/写入/删除/修改）；
//   - verifying：验证/复测类（只读验证、复测、核对）。
func actionClass(s Signal) string {
	kind := strings.ToLower(s.Kind)
	if kind == "verify" {
		return "verifying"
	}
	if kind == "exploit" {
		return "mutating"
	}
	for _, a := range s.Actions {
		la := strings.ToLower(a)
		for _, kw := range []string{"exploit", "brute", "write", "delete", "modify", "bypass", "inject", "重放", "爆破", "利用"} {
			if strings.Contains(la, kw) {
				return "mutating"
			}
		}
	}
	if len(s.Actions) > 0 {
		return "verifying"
	}
	return "" // 结构化不足
}

// assetIntersection 返回两意图锚定资产的交集（升序）。
func assetIntersection(a, b Signal) []int64 {
	set := map[int64]bool{}
	for _, id := range a.AssetIDs {
		set[id] = true
	}
	out := []int64{}
	for _, id := range b.AssetIDs {
		if set[id] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// toolIntersection 返回工具交集大小由调用方算——这里只做集合交（沿用资产模式）。
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// conflictRules 是代码常量规则注册表（顺序即检测顺序；新增规则加在这里 + 单测）。
var conflictRules = []Rule{
	{
		// 同资产互斥：同一资产上一个 mutating 一个 verifying 意图并发——
		// 验证先行（证据保全型 > 状态改变型，代码常量纪律）。
		ID: "asset_mutex",
		Detect: func(a, b Signal) (map[string]any, bool) {
			shared := assetIntersection(a, b)
			if len(shared) == 0 {
				return nil, false
			}
			ca, cb := actionClass(a), actionClass(b)
			if ca == "" || cb == "" || ca == cb {
				return nil, false
			}
			return map[string]any{
				"asset_ids":    shared,
				"class_a":      ca,
				"class_b":      cb,
				"a_mutating":   ca == "mutating",
				"b_mutating":   cb == "mutating",
			}, true
		},
		TieBreak: func(a, b Signal) (string, string) {
			ca, cb := actionClass(a), actionClass(b)
			if ca == "verifying" && cb == "mutating" {
				return "a", "验证先行：verify 类意图先于 exploit 类执行（证据保全型优先于状态改变型，代码常量纪律）"
			}
			if cb == "verifying" && ca == "mutating" {
				return "b", "验证先行：verify 类意图先于 exploit 类执行（证据保全型优先于状态改变型，代码常量纪律）"
			}
			return "", ""
		},
	},
	{
		// 同目标反向谓词：同资产交集 + 同漏洞类 + 摘要对偶词——一方主张存在、
		// 另一方主张不存在。对偶词表保守起步（宁漏勿误）。
		ID: "reverse_predicate",
		Detect: func(a, b Signal) (map[string]any, bool) {
			shared := assetIntersection(a, b)
			if len(shared) == 0 || a.VulnClass == "" || a.VulnClass != b.VulnClass {
				return nil, false
			}
			pa, pb := negates(a.Summary), negates(b.Summary)
			if pa == pb {
				return nil, false
			}
			return map[string]any{
				"asset_ids":  shared,
				"vulnclass":  a.VulnClass,
				"a_positive": !pa,
				"b_positive": !pb,
			}, true
		},
		TieBreak: func(a, b Signal) (string, string) {
			// 同级主张冲突没有确定性优先序——交 arbiter 调研（证据更强的赢）。
			return "", ""
		},
	},
	{
		// 资源冲突：同为 mutating 且共用同一工具（同一 Burp 会话/同一重放通道
		// 并发会互相污染会话态）——串行化，先到先得（priority 高者先）。
		ID: "resource_clash",
		Detect: func(a, b Signal) (map[string]any, bool) {
			if actionClass(a) != "mutating" || actionClass(b) != "mutating" {
				return nil, false
			}
			shared := assetIntersection(a, b)
			if len(shared) == 0 {
				return nil, false
			}
			sharedTools := []string{}
			for _, t := range a.Actions {
				if containsFold(b.Actions, t) {
					sharedTools = append(sharedTools, t)
				}
			}
			if len(sharedTools) == 0 {
				return nil, false
			}
			return map[string]any{
				"asset_ids":    shared,
				"shared_tools": sharedTools,
			}, true
		},
		TieBreak: func(a, b Signal) (string, string) {
			return "", "" // 并发冲突无内容级优先序——交 arbiter
		},
	},
}

// negationWords 是反向谓词检测的对偶词表（保守起步——宁漏勿误）。
var negationWords = []string{"无越权", "不存在", "无漏洞", "无法利用", "未发现", "不可利用", "no authz", "not exploitable"}

// negates 判断摘要是否为否定主张。
func negates(summary string) bool {
	ls := strings.ToLower(summary)
	for _, w := range negationWords {
		if strings.Contains(ls, w) {
			return true
		}
	}
	return false
}

// Scan 对信号集做两两冲突检测（排序归一防 A/B 重复登记）。返回全部命中。
func Scan(signals []Signal) []Found {
	out := []Found{}
	for i := 0; i < len(signals); i++ {
		for j := i + 1; j < len(signals); j++ {
			a, b := signals[i], signals[j]
			if len(a.AssetIDs) == 0 || len(b.AssetIDs) == 0 {
				continue // 无锚不参与（结构化不足，宁漏勿误）
			}
			for _, r := range conflictRules {
				basis, hit := r.Detect(a, b)
				if !hit {
					continue
				}
				f := Found{RuleID: r.ID, Basis: basis, SideA: a, SideB: b}
				f.Winner, f.Reason = r.TieBreak(a, b)
				out = append(out, f)
			}
		}
	}
	return out
}

// NormalizePair 返回归一对（小 id 在前）——与 db.conflictPair 同口径（双包各自
// 实现，行为由测试钉死一致）。
func NormalizePair(a, b int64) (int64, int64) {
	if a <= b {
		return a, b
	}
	return b, a
}

// RuleIDs 返回注册表全部规则 ID（server 侧审计/展示用）。
func RuleIDs() []string {
	out := make([]string, 0, len(conflictRules))
	for _, r := range conflictRules {
		out = append(out, r.ID)
	}
	return out
}
