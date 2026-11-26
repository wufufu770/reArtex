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
//
// 粗筛（B-04B，CandidateAssetsByTokens）：存量 assets.technologies 是原始描述串
// （如 "Apache Log4j 2.14.0"），GIN 索引 idx_av2_technologies 的 @> 只服务
// 「数组元素精确等值」，对归一化 token（log4j）必然 miss——故粗筛采用卡内并列
// 选项的 token 子串包含（position(...IN lower(tech))，无 LIKE 通配符语义）：
//   - 刻意过包含：log4j 同时命中 "log4j-core 2.17.0"——粗筛只产候选，精判
//     （B-04C 经 B-05 ranges 纯求值）负责收敛，粗筛宁滥勿缺；
//   - type='service' 前置过滤与部分 GIN 索引同谓词、与数据现实同界（只有
//     HTTP 服务资产写 technologies）；子串包含不走索引是如实申报的取舍
//     （资产表规模有界 + LIMIT 封顶，正确性优先于索引）。

import (
	"fmt"
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

// techTokenQueryForm 校验 token 满足 NormalizeTechToken 的输出合同：
// 首字符小写字母、其余 [a-z0-9-]、长度 ≥2。这是粗筛查询的输入闸——上游若绕过
// 规范化直接传原始文本，必须 fail-closed 报错（绝不静默放行怪形态进 SQL 参数），
// 也不构造任何通配符语义（查询用 position 字面包含，非 LIKE）。
func techTokenQueryForm(token string) bool {
	if len(token) < 2 || token[0] < 'a' || token[0] > 'z' {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// candidateAssetsMaxTokens 限制单次粗筛的 token 数（多 token 逐条查询，上限防
// 上游失控展开成查询风暴）。超限 fail-closed 报错。
const candidateAssetsMaxTokens = 32

// candidateAssetsMaxLimit 单 token 候选上限硬顶（卡要求 4 的节流前移到候选层：
// 提前封顶单 token 命中量，防超大资产表下粗筛失控）。超限报错不静默夹紧。
const candidateAssetsMaxLimit = 1000

// CandidateAssetsByTokens 按归一化产品 token 粗筛候选资产（B-04B）：在
// assets.technologies（TEXT[]）里做 token 子串包含匹配，返回候选资产全行。
// 只做粗筛不做版本精判——命中是「候选」不是「受影响」，判定属 B-04C。
//   - tokens 必须全部满足 NormalizeTechToken 输出合同（techTokenQueryForm），
//     否则报错（含原始 token 文本，fail-closed）；
//   - limit 必须 >0 且 ≤1000（显式参数，零默认夹带）；
//   - 同一资产命中多个 token 时去重只保留首见；limit 是【合并结果】的全局封顶
//     （按 token 切片顺序逐 token 查询、先到先得，超额即止不再发后续查询）——
//     确定性输出：组内 last_seen DESC, id DESC 与 QueryByType 同序；
//   - 空 token 列表返回 (nil, nil)，不发查询。
func (s *AssetStore) CandidateAssetsByTokens(tokens []string, limit int) ([]*Asset, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("cve_match: limit 必须 >0，得到 %d", limit)
	}
	if limit > candidateAssetsMaxLimit {
		return nil, fmt.Errorf("cve_match: limit %d 超过硬顶 %d", limit, candidateAssetsMaxLimit)
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !techTokenQueryForm(t) {
			return nil, fmt.Errorf("cve_match: token %q 不满足规范化输出合同（须先过 NormalizeTechToken）", t)
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	if len(uniq) > candidateAssetsMaxTokens {
		return nil, fmt.Errorf("cve_match: token 数 %d 超上限 %d", len(uniq), candidateAssetsMaxTokens)
	}

	merged := make([]*Asset, 0, len(uniq))
	byID := map[int64]bool{}
	for _, token := range uniq {
		if len(merged) >= limit {
			break // 全局封顶：合并结果已满，不再发后续 token 查询
		}
		rows, err := s.db.Query(`
SELECT id, type, company_id, array_to_json(task_ids)::text,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''),
       COALESCE(c_segment::text,''), port,
       COALESCE(icp,''), array_to_json(bound_domains)::text, array_to_json(open_ports)::text, COALESCE(record_type,''),
       array_to_json(record_value)::text, COALESCE(bundle_id,''), COALESCE(app_name,''),
       COALESCE(category,''), COALESCE(app_description,''), COALESCE(app_icp,''),
       COALESCE(url,''), COALESCE(service_type,''), COALESCE(service_name,''),
       COALESCE(favicon_mmh3,''), status_code, content_length,
       COALESCE(page_title,''), array_to_json(technologies)::text, array_to_json(auth)::text,
       COALESCE(method,''), array_to_json(params)::text, extra, last_seen::text
FROM assets
WHERE type = 'service'
  AND EXISTS (SELECT 1 FROM unnest(technologies) AS tech WHERE position($1 IN lower(tech)) > 0)
ORDER BY last_seen DESC, id DESC
LIMIT $2`, token, limit)
		if err != nil {
			return nil, err
		}
		hits, scanErr := scanAssets(rows)
		_ = rows.Close() // 循环内查询：显式关（scanAssets 不代关，defer 会积压）
		if scanErr != nil {
			return nil, scanErr
		}
		for _, a := range hits {
			if byID[a.ID] {
				continue // 多 token 同资产：去重首见
			}
			if len(merged) >= limit {
				break // 全局封顶：本 token 命中已够，保留顺序先到先得
			}
			byID[a.ID] = true
			merged = append(merged, a)
		}
	}
	return merged, nil
}
