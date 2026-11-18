package db

import (
	"net"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// 归一化自然键 (nkey)：移植自旧 graph/id.go，去掉 StableID 哈希（PG 用 BIGSERIAL 主键 +
// UNIQUE(type, nkey) 去重）。0.15.15 清理：仅保留可达的键构造与 URL 解析助手——
// 旧层级编码键（IPKey/PortKey/SiteKey/ServiceKey/ParameterKey/TechKey/SplitURL/
// NormalizeParamName/itoa）在图存储迁移到 BIGSERIAL 主键后已无任何调用方
// （deadcode 可达性分析 + 全仓含测试 grep 双重确认零引用）。

func DomainKey(fqdn string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
}

// RootDomain returns the registrable domain (eTLD+1) for a host and whether the
// host itself IS that apex (§3.1). Edge cases (§3.1 边界处理): an IP literal or a
// host publicsuffix can't classify (localhost / internal / non-ICANN TLD) is
// returned unchanged as its own root with isApex=true — best-effort, never treated
// as a subdomain.
func RootDomain(host string) (root string, isApex bool) {
	h := DomainKey(host)
	if h == "" || net.ParseIP(h) != nil {
		return h, true
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil || etld1 == "" {
		return h, true
	}
	return etld1, h == etld1
}

var (
	reNumeric = regexp.MustCompile(`^\d+$`)
	reUUID    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex     = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reLong    = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
)

// TemplatePath collapses high-cardinality path segments into placeholders so the
// graph is not flooded by instances: /user/123 -> /user/{id}.
func TemplatePath(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		switch {
		case s == "":
			continue
		case reNumeric.MatchString(s):
			segs[i] = "{id}"
		case reUUID.MatchString(s):
			segs[i] = "{uuid}"
		case reHex.MatchString(s):
			segs[i] = "{hex}"
		case reLong.MatchString(s):
			segs[i] = "{token}"
		}
	}
	return strings.Join(segs, "/")
}

func defaultPort(scheme, p string) int {
	if p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	switch scheme {
	case "https":
		return 443
	case "http":
		return 80
	default:
		return 0
	}
}
