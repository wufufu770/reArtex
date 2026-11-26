package db

import (
	"strings"
	"testing"
)

// B-04A 验收表驱动：Apache Log4j、nginx、OpenSSL、带 vendor 前缀、无版本、
// 版本在产品名内、大小写/空格变体、任意描述文本；不误合并不同产品。
func TestNormalizeTechToken(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		product string
		version string
		ok      bool
	}{
		// 卡验收点名形态
		{"卡例：Apache Log4j 2.14.0", "Apache Log4j 2.14.0", "log4j", "2.14.0", true},
		{"nginx 无版本", "nginx", "nginx", "", true},
		{"OpenSSL 大小写", "OpenSSL", "openssl", "", true},
		{"vendor 前缀 Apache", "Apache Tomcat 9.0.83", "tomcat", "9.0.83", true},
		{"vendor 前缀叠加剥离", "Apache Eclipse Jetty", "jetty", "", true},
		{"无版本产品词组", "Spring Boot", "springboot", "", true},
		{"版本在产品名内（点分可切）", "log4j2.17.0", "log4j", "2.17.0", true},
		{"裸 log4j2 保守不切（数字是名字一部分）", "Log4j2", "log4j2", "", true},
		{"版本内嵌点分 nginx1.25.3", "nginx1.25.3", "nginx", "1.25.3", true},
		{"openssl3 主版本", "openssl3", "openssl", "3", true},
		{"v 前缀版本", "nginx v1.25", "nginx", "v1.25", true},
		{"连字符产品名保留", "log4j-core", "log4j-core", "", true},
		{"连字符+版本", "log4j-core 2.17.0", "log4j-core", "2.17.0", true},
		{"分隔变体归一 spring_boot", "spring_boot", "springboot", "", true},
		{"分隔变体归一 open/ssl", "Open SSL", "openssl", "", true},
		{"下划线+版本", "wordpress_6.4", "wordpress", "6.4", true},

		// 含糊文本：拒绝提取，绝不猜（Raw 保留）
		{"整句描述拒绝", "runs the latest version of their stack", "", "", false},
		{"多版本拒绝", "log4j 2.14.0 and 2.15.0", "", "", false},
		{"多词混数字词拒绝", "log4j 2.14.0 extra", "", "", false},
		{"纯厂商词拒绝", "apache apache", "", "", false},
		{"数字残片拒绝", "2.x", "", "", false},
		{"空串拒绝", "   ", "", "", false},
		{"超长描述拒绝", "a b c d e f g", "", "", false},

		// 数字本就是名字一部分：护栏不误切
		{"sha256 不切", "sha256", "sha256", "", true},
		{"utf8mb4 不切", "utf8mb4", "utf8mb4", "", true},
		{"sha1 不切", "SHA1", "sha1", "", true},
		{"base64 不切", "base64", "base64", "", true},
		{"ipv6 不切", "ipv6", "ipv6", "", true},

		// 相似产品不误合并（卡要求 5）
		{"log4j ≠ log4j-core", "log4j-core 2.17.0", "log4j-core", "2.17.0", true},
	}
	for _, c := range cases {
		got := NormalizeTechToken(c.in)
		if got.OK != c.ok || got.Product != c.product || got.Version != c.version {
			t.Fatalf("%s: NormalizeTechToken(%q) = {Product:%q Version:%q OK:%v}, want {Product:%q Version:%q OK:%v}",
				c.name, c.in, got.Product, got.Version, got.OK, c.product, c.version, c.ok)
		}
		if !c.ok && got.Raw != strings.TrimSpace(c.in) {
			t.Fatalf("%s: 失败结果必须保留 Raw 原值（去首尾空白），得到 %q", c.name, got.Raw)
		}
	}
}

// 确定性：同输入恒同输出（连调两次逐字段相等）。
func TestNormalizeTechTokenDeterministic(t *testing.T) {
	for _, in := range []string{"Apache Log4j 2.14.0", "nginx", "runs the whole stack", "log4j2"} {
		a := NormalizeTechToken(in)
		b := NormalizeTechToken(in)
		if a != b {
			t.Fatalf("NormalizeTechToken(%q) 非确定性: %+v vs %+v", in, a, b)
		}
	}
}

// 相似产品不同 token：不同输入不得归一到同一 product（卡要求 5 的反向断言）。
func TestNormalizeTechTokenNoCrossMerge(t *testing.T) {
	pairs := [][2]string{
		{"nginx", "nginx plus"},
		{"log4j", "log4j-core"},
		{"openssl", "openssh"},
		{"spring", "springboot"}, // spring 与 springboot 是不同 token（仅拼写归一，不跨产品）
	}
	for _, p := range pairs {
		a := NormalizeTechToken(p[0])
		b := NormalizeTechToken(p[1])
		if a.OK && b.OK && a.Product == b.Product {
			t.Fatalf("不同产品被误合并: %q 与 %q 都归一到 %q", p[0], p[1], a.Product)
		}
	}
}
