package evidence

// finding_dedup（0.15.35，批次 G1）：finding 的 root_cause_key —— 跨端点同类
// 漏洞的「疑似同根因」合并标记键。
//
// 裁决：**自动标记，不自动合并**。同 root_cause_key 的 findings 在列表中聚合
// 成组（affected_locations 同组展示），合并操作走人工——自动合并会破坏复核流
// （提出人/验证人各自独立登记的记录有独立审计链），且把多个已验证记录折叠成
// 一个写操作是高后果写。标记是提议，合并是人权。
//
// 键 = hash(vulnclass | host | 归一化路径模式)：
//   - 归一化：URL query 全剥（query 参数是「端点变体」不是「根因差异」）；
//     路径段中的数字/UUID/长 token → {var}（/api/v1/item/123 与
//     /api/v1/item/456 是同一根因的两个 affected_locations）。
//   - host 保留在键里（不同 host 的同类注入不合并）。

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	variantSegRe  = regexp.MustCompile(`^(?:\d+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[A-Za-z0-9_-]{22,})$`)
	variantWordRe = regexp.MustCompile(`\b\d+\b`)
)

// RootCauseKey 计算 finding 的根因标记键。url 形如 https://host/path?query。
func RootCauseKey(vulnclass, url string) string {
	host, path := splitHostPath(url)
	return hashRootCause(vulnclass, host, NormalizePathPattern(path))
}

// NormalizePathPattern 把路径段中的变体成分（数字/UUID/长 token）归一化为
// {var}——/api/v1/item/123 与 /api/v1/item/456 归出同一模式。
func NormalizePathPattern(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segs {
		if variantSegRe.MatchString(seg) {
			segs[i] = "{var}"
		}
	}
	return strings.Join(segs, "/")
}

// splitHostPath 拆 URL；非 URL 输入按原文当 path（host 空）。
func splitHostPath(path string) (host, rest string) {
	u := strings.TrimSpace(path)
	if i := strings.Index(u, "://"); i >= 0 {
		rest = u[i+3:]
		j := strings.IndexAny(rest, "/?#")
		if j >= 0 {
			host = rest[:j]
			rest = rest[j:]
		}
	}
	rest = strings.SplitN(rest, "?", 2)[0]
	rest = strings.TrimSuffix(rest, "#")
	return host, rest
}

func hashRootCause(vulnclass, host, pathPattern string) string {
	h := sha256.Sum256([]byte(vulnclass + "|" + host + "|" + pathPattern))
	return "rc_" + dedupHex(h) + ""
}

func dedupHex(h [32]byte) string {
	return hex.EncodeToString(h[:])[:16]
}

// RootCauseKeyFromEvidence 是 evidence 里抓 URL 归因的兜底：finding 没有结构化
// URL 字段（evidence 是自由文本），从 evidence 首条 URL 抽取。找不到 → 空键
//（不参与合并标记——自由文本证据归因不可靠，宁缺勿错）。
func RootCauseKeyFromEvidence(vulnClass, evidence string) string {
	u := firstURL(evidence)
	if u == "" {
		return ""
	}
	return RootCauseKey(vulnClass, u)
}

var urlRe = regexp.MustCompile(`https?://[^\s"'<>\)\]]+`)

func firstURL(s string) string {
	m := urlRe.FindString(s)
	return strings.TrimRight(m, ".,;:")
}
