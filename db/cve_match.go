package db

// cve_match.go：资产技术栈 × CVE 情报反向匹配（原卡 B-04）。
//
// 本文件当前只承载 B-04A 的【token 规范化纯函数】——不访问 DB、不连 B-05 判定、
// 不写 hint、不做任何 IO（后续子卡 B-04B/C 在同文件追加查询与精判接线时再扩展）。
//
// 纪律（B-04A 卡要求逐条对应）：
//   - 确定性：同输入恒同输出，无随机、无 map 遍历序依赖。
//   - 保守：只解析「可识别」的形态（小写化、空格/分隔符归一、显式厂商前缀表、
//     尾部版本模式）；含糊描述文本（整句话、多个版本号、无产品词）一律 OK=false，
//     绝不对含糊文本猜产品。Raw 原样保留，调用方永远能追溯原始值。
//   - 不误合并：log4j-core 与 log4j 是不同 token（分隔符内嵌的产品词不折叠），
//     nginx 与 nginx plus 是不同 token；只有「同一产品的拼写变体」才归一到同一
//     token（大小写、空格、下划线/斜杠分隔、厂商前缀）。
//
// 版本提取护栏（避免把产品名的一部分误切成本）：
//   - 独立尾词版本：`v?数字(.数字)*`（2.14.0 / v3 / 1.25）原样保留点分形态。
//   - 点分内嵌版本（nginx1.25.3 / log4j2.17.0）：点分形态无歧义，基名 ≥2 字符
//     即切分。
//   - 无点单数字后缀（openssl3 / tomcat9）：仅当基名 ≥4 个纯字母才切——
//     sha256（基名 3 字母）、base64（两位尾数）、utf8mb4（基名含数字）、log4j2
//     （基名含数字）全部不切，保守保留整词为产品名。
//   - 残余风险如实申报：裸 log4j2 形态因此不出版本（点分 log4j2.17.0 能出）；
//     该保守取舍把「误切产品名」与「漏出版本」交换为后者——漏版本在 hint 链路
//     里只会落到 maybe（B-05 纪律），不会产生假 affected。

import (
	"strings"
)

// TechToken 是技术栈字符串的规范化结果。OK=false 表示无法确信地提取产品名
// （含糊/描述性文本）——Product/Version 为空，Raw 永远保留原值。
type TechToken struct {
	Raw     string // 原始输入（trim 后原样保留）
	Product string // 归一化产品 token（小写、去厂商前缀、去分隔变体）；OK=false 时为空
	Version string // 提取到的版本（如 "2.14.0"/"v3"/"2"）；未识别为空
	OK      bool   // true=成功提取产品名；false=无法提取（绝不猜）
}

// techVendorPrefixes 是显式厂商/命名空间前缀表（小写、词形原样）。保守起见只收
// 确定是厂商前缀的词——绝不做「疑似前缀」猜测。前缀剥离循环进行直到不再变化
// （"Apache Eclipse Jetty" 这类叠加前缀也收敛）。
var techVendorPrefixes = map[string]bool{
	"apache":    true,
	"eclipse":   true,
	"pivotal":   true,
	"oracle":    true,
	"microsoft": true,
	"gnu":       true,
	"the":       true,
}

// NormalizeTechToken 把一条资产技术栈描述规范化成（产品 token, 可选版本）。
// 纯函数：不访问 DB、无副作用、同输入恒同输出。
func NormalizeTechToken(raw string) TechToken {
	out := TechToken{Raw: strings.TrimSpace(raw)}
	if out.Raw == "" {
		return out
	}
	// 分隔符归一：下划线/斜杠/逗号/分号/冒号/括号/方括号 → 空格，再按空白切词。
	// 连字符刻意不切：log4j-core / spring-boot 这类连字符是产品名的一部分，
	// 折叠它们会把不同产品合并成一个 token（卡要求 5 的「不误合并」）。
	normalized := out.Raw
	for _, r := range []rune{'_', '/', ',', ';', ':', '(', ')', '[', ']', '"', '\''} {
		normalized = strings.ReplaceAll(normalized, string(r), " ")
	}
	words := strings.Fields(normalized)
	// 描述性文本护栏：技术栈条目通常 1-5 个词；更长的按含糊文本拒绝（绝不猜）。
	if len(words) == 0 || len(words) > 5 {
		return out
	}
	// 多版本护栏：任意位置出现 ≥2 个版本形态词 ⇒ 无法确定指哪个，拒绝
	// （"log4j 2.14.0 and 2.15.0" 一类描述绝不猜）。
	versionForms := 0
	for _, w := range words {
		if techVersionWordForm(w) {
			versionForms++
		}
	}
	if versionForms > 1 {
		return out
	}
	// 尾部独立版本词：`v?数字(.数字)*`（2.14.0 / v3 / 1.25），从尾部连续剥离。
	version := ""
	for len(words) > 0 && techVersionWordForm(words[len(words)-1]) {
		version = words[len(words)-1]
		words = words[:len(words)-1]
	}
	// 厂商前缀剥离（循环直到不再变化；剥离后为空 ⇒ 全是厂商词，拒绝）。
	for len(words) > 0 && techVendorPrefixes[strings.ToLower(words[0])] {
		words = words[1:]
	}
	if len(words) == 0 {
		return out
	}
	// 多词产品护栏：剩余词（≥2 个）必须全是字母/连字符词——多词里混着数字词
	// 说明是描述性文本（"log4j 2.14.0 extra"），拒绝；单词产品允许内嵌数字
	// （utf8mb4 / sha256 / base64 本就是产品名的一部分）。
	if len(words) > 1 {
		for _, w := range words {
			if !techAlphaWordForm(w) {
				return out
			}
		}
	}
	// 产品词拼接：小写、去掉词间空白（"Spring Boot" 与 "springboot" 归一）。
	product := strings.ToLower(strings.Join(words, ""))
	// 内嵌尾部版本（nginx1.25.3 / openssl3 / log4j2.17.0）：护栏见文件头注释。
	if base, ver, ok := techSplitEmbeddedVersion(product); ok {
		product, version = base, ver
	}
	// 产品词必须以字母开头且非空：以数字开头（"2.x"、"-3.0" 残片）⇒ 解析失败，
	// 拒绝提取——绝不把残片当产品名。
	if product == "" || product[0] < 'a' || product[0] > 'z' {
		return out
	}
	out.Product = product
	out.Version = version
	out.OK = true
	return out
}

// techAlphaWordForm 判断词是否纯字母/连字符（多词产品的合法性检查用）。
func techAlphaWordForm(w string) bool {
	if w == "" {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c >= 'A' && c <= 'Z' {
			continue
		}
		if c >= 'a' && c <= 'z' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// techVersionWordForm 判断一个词是否是独立版本词形态：可选 v 前缀 + 点分纯数字
// （2.14.0 / v3 / 1.25）。2.x / >=1.0 这类不是（含非数字段），不算版本词。
func techVersionWordForm(w string) bool {
	w = strings.ToLower(w)
	if w == "" {
		return false
	}
	if w[0] == 'v' && len(w) > 1 {
		w = w[1:]
	}
	if w[0] < '0' || w[0] > '9' {
		return false
	}
	for _, seg := range strings.Split(w, ".") {
		if seg == "" {
			return false // "1..2" / 尾点
		}
		for _, c := range seg {
			if c < '0' || c > '9' {
				return false // 2.x / 1.0b
			}
		}
	}
	return true
}

// techSplitEmbeddedVersion 处理「版本长在产品名里」的形态，按后缀是否点分分级：
//   - 点分后缀（无歧义版本）：nginx1.25.3 → (nginx, 1.25.3)、log4j2.17.0 →
//     (log4j, 2.17.0)——基名 ≥2 字符即可切。
//   - 无点单数字后缀（疑似主版本）：openssl3 → (openssl, 3)、tomcat9 →
//     (tomcat, 9)——要求基名 ≥4 个【纯字母】且不含数字；base64 / utf8mb4 /
//     sha256 / log4j2 这类「数字本就是名字一部分」的词全部不切，保守保留整词
//     为产品名（如实申报：log4j2 的裸形态因此不出版本——点分形态 log4j2.17.0
//     能出，B-04B 粗筛可用整词 token，不影响 hint 链路安全性）。
func techSplitEmbeddedVersion(product string) (base, version string, ok bool) {
	// 定位最后一个字母：其后全是数字/点才是候选版本后缀。
	last := -1
	for i := 0; i < len(product); i++ {
		if product[i] >= 'a' && product[i] <= 'z' {
			last = i
		}
	}
	if last < 0 || last == len(product)-1 {
		return "", "", false // 无字母或尾部无数字
	}
	base = product[:last+1]
	tail := product[last+1:]
	if !techVersionWordForm(tail) {
		return "", "", false
	}
	if strings.Contains(tail, ".") {
		if len(base) < 2 {
			return "", "", false
		}
		return base, tail, true
	}
	// 无点形态：单数字 + 基名 ≥4 纯字母（防 sha256/base64/log4j2 类误切）。
	if len(tail) != 1 || len(base) < 4 {
		return "", "", false
	}
	if !techAlphaWordForm(base) {
		return "", "", false
	}
	return base, tail, true
}
