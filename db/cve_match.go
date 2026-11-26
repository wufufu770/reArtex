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
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/cveintel"
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

// IsValidTechTokenQuery 导出 token 输入合同判定（单一真相源：server 批次生产方
// 在组 CVEMatchInput 前用它过滤产品 token，保证不把非法 token 送进
// MatchCVEToAssets 的 fail-closed 闸——规则与 techTokenQueryForm 完全一致）。
func IsValidTechTokenQuery(token string) bool {
	return techTokenQueryForm(token)
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

// ---------------------------------------------------------------------------
// B-04C：版本精判（粗筛候选 × B-05 ranges 纯求值）
// ---------------------------------------------------------------------------

// RangeVerdict 是 B-05 三态的本地命名（与侧车 version 包同值：affected /
// not_affected / maybe）。侧车是独立 module 不可 import，这里只做值对齐——
// 绝不引入第四态，判定语义完全由 B-05 纯求值器产出，本包零复制判定逻辑。
type RangeVerdict string

const (
	VerdictAffected    RangeVerdict = "affected"
	VerdictNotAffected RangeVerdict = "not_affected"
	VerdictMaybe       RangeVerdict = "maybe"
)

// VersionRangeEvaluator 是 B-05 精判的最小抽象：与 cveintel.Client.EvaluateRanges
// 同签名，真实客户端直接满足（编译期断言见 cve_match 链），测试注入 fake。
type VersionRangeEvaluator interface {
	EvaluateRanges(ctx context.Context, ecosystem, version string, ranges []cveintel.Range) (verdict, reason string, err error)
}

// matchCandidatesLimit 是 MatchCVEToAssets 单次粗筛候选封顶（节流前移，卡要求 4
// 的 hint 风暴保护在候选层先收口）。
const matchCandidatesLimit = 500

// CVEMatchInput 是一次反向匹配的输入（卡骨架 CVEMatchInput）。
//   - ProductTokens 必须全部来自 NormalizeTechToken 输出（粗筛合同闸会校验）；
//   - Ranges 复用 B-01D 客户端的 cveintel.Range（键与 B-03D 纯求值适配层一致，
//     零复制区间结构）；允许为空——此时全部产品命中候选判 maybe（无区间可判
//     = 信息不足，B-05 fail-closed 同向）；
//   - Ecosystem 是顶层组件生态（来自 CVE 情报记录）；空或侧车未知生态时纯求值
//     fail-closed 落 maybe，绝不 not_affected。
type CVEMatchInput struct {
	CVEID         string
	Ecosystem     string
	ProductTokens []string
	Ranges        []cveintel.Range
}

// AssetMatch 是一个候选资产的精判结果（卡骨架 AssetMatch）。
//   - AssetDomain 是资产标识（domain → url → ip → "#id" 兜底）；
//   - Verdict 只会是 affected 或 maybe（not_affected 已在返回前剔除）；
//   - MatchedTech/MatchedProduct/MatchedVersion/Reason 供 B-04F hint 渲染与
//     审计追溯（卡骨架之外的增加字段，如实申报）；
//   - TaskIDs 由 B-04D 子卡补齐（资产→未终态任务归属）。
type AssetMatch struct {
	AssetID        int64
	AssetDomain    string
	Technologies   []string
	Verdict        RangeVerdict
	MatchedTech    string
	MatchedProduct string
	MatchedVersion string
	Reason         string
	TaskIDs        []int64
}

// assetMatchIdentifier 取资产标识串。
func assetMatchIdentifier(a *Asset) string {
	switch {
	case a.Domain != "":
		return a.Domain
	case a.URL != "":
		return a.URL
	case a.IP != "":
		return a.IP
	default:
		return fmt.Sprintf("#%d", a.ID)
	}
}

// matchedTechToken 在资产的 technologies 里找「归一化产品命中输入 token 集」的
// 最佳条目：优先取带版本的命中（保持 technologies 原始顺序，确定性）；全部无
// 版本时取第一个命中。无命中返回 OK=false——粗筛子串过包含的候选（如 log4j
// 命中 "log4j-core 2.17.0"）在此被产品等值收敛剔除，正是 B-04A「不误合并」
// 纪律在精判层的落点。
func matchedTechToken(a *Asset, tokens map[string]bool) (TechToken, bool) {
	var withVer, noVer TechToken
	for _, tech := range a.Technologies {
		tt := NormalizeTechToken(tech)
		if !tt.OK || !tokens[tt.Product] {
			continue
		}
		if tt.Version != "" {
			if withVer.Product == "" {
				withVer = tt
			}
			continue
		}
		if noVer.Product == "" {
			noVer = tt
		}
	}
	if withVer.Product != "" {
		return withVer, true
	}
	if noVer.Product != "" {
		return noVer, true
	}
	return TechToken{}, false
}

// MatchCVEToAssets 反向匹配全链（卡骨架）：①粗筛候选（B-04B 子串包含）；
// ②逐候选产品等值收敛 + B-05 ranges 纯求值精判（经注入的 eval，单体零复制
// 判定逻辑）；③剔除 not_affected，只返回 affected/maybe。
//   - eval 为 nil ⇒ 报错（精判绝不静默跳过）；
//   - 求值出错（unavailable/protocol）⇒ 整体返回错误——「网络失败」绝不解释成
//     「没有漏洞」，宁可整批不产 hint（B-04G 批处理记日志后跳过本批）；
//   - 资产侧无版本号 ⇒ maybe（卡明文：不判 affected）；CVE 侧无区间 ⇒ 全部
//     产品命中候选 maybe；
//   - 候选封顶 matchCandidatesLimit；确定性：候选序 = 粗筛序（token 切片顺序），
//     同资产命中条目 = technologies 原始顺序优先带版本者。
func (d *DB) MatchCVEToAssets(ctx context.Context, eval VersionRangeEvaluator, in CVEMatchInput) ([]AssetMatch, error) {
	if eval == nil {
		return nil, fmt.Errorf("cve_match: 精判器 eval 必填（绝不静默跳过精判）")
	}
	if strings.TrimSpace(in.CVEID) == "" {
		return nil, fmt.Errorf("cve_match: CVEID 必填")
	}
	tokenSet := make(map[string]bool, len(in.ProductTokens))
	for _, t := range in.ProductTokens {
		if !techTokenQueryForm(t) {
			return nil, fmt.Errorf("cve_match: token %q 不满足规范化输出合同（须先过 NormalizeTechToken）", t)
		}
		tokenSet[t] = true
	}
	if len(tokenSet) == 0 {
		return nil, nil
	}

	candidates, err := d.Assets().CandidateAssetsByTokens(in.ProductTokens, matchCandidatesLimit)
	if err != nil {
		return nil, err
	}

	out := make([]AssetMatch, 0, len(candidates))
	for _, a := range candidates {
		tt, ok := matchedTechToken(a, tokenSet)
		if !ok {
			continue // 产品等值收敛：子串过包含但归一化产品不在 token 集 ⇒ 剔除
		}
		verdict, reason := VerdictMaybe, ""
		if tt.Version == "" {
			reason = "资产指纹无版本号，判 maybe（绝不判 affected）"
		} else if len(in.Ranges) == 0 {
			reason = "CVE 情报无影响区间，判 maybe"
		} else {
			v, r, err := eval.EvaluateRanges(ctx, in.Ecosystem, tt.Version, in.Ranges)
			if err != nil {
				return nil, fmt.Errorf("cve_match: CVE %s 精判资产 #%d（%s 版本 %s）失败: %w", in.CVEID, a.ID, tt.Product, tt.Version, err)
			}
			switch RangeVerdict(v) {
			case VerdictAffected, VerdictNotAffected, VerdictMaybe:
				verdict, reason = RangeVerdict(v), r
			default:
				return nil, fmt.Errorf("cve_match: 精判返回非法三态 %q（B-05 契约破坏）", v)
			}
		}
		if verdict == VerdictNotAffected {
			continue // 卡骨架步骤 3：只返回 verdict != not_affected
		}
		out = append(out, AssetMatch{
			AssetID:        a.ID,
			AssetDomain:    assetMatchIdentifier(a),
			Technologies:   a.Technologies,
			Verdict:        verdict,
			MatchedTech:    tt.Raw,
			MatchedProduct: tt.Product,
			MatchedVersion: tt.Version,
			Reason:         reason,
		})
	}
	if len(out) == 0 {
		return out, nil
	}
	// B-04D：批量回填资产→未终态任务归属（一次联查，绝不逐资产往返）。
	ids := make([]int64, 0, len(out))
	for i := range out {
		ids = append(ids, out[i].AssetID)
	}
	taskMap, err := d.Assets().NonTerminalTasksByAssets(ids)
	if err != nil {
		return nil, fmt.Errorf("cve_match: 资产→任务归属查询失败: %w", err)
	}
	for i := range out {
		out[i].TaskIDs = taskMap[out[i].AssetID]
	}
	return out, nil
}

// 编译期断言：真实 cveintel 客户端直接满足精判抽象（B-04C 经真实 sidecar 链）。
var _ VersionRangeEvaluator = (*cveintel.Client)(nil)

// ---------------------------------------------------------------------------
// B-04D：资产→未终态任务归属（卡明文「给定资产查未终态任务」的现成查询此前
// 不存在，本子卡新建）
// ---------------------------------------------------------------------------

// 未终态任务白名单：created/running/paused（done/failed/timeout 是终态）。
// 终态任务不再接受 hint；软删除（deleted_at）一律排除。SQL 内联白名单而非
// 「NOT IN 终态黑名单」：新增状态默认不收（fail-closed），卡纪律「最小 scope」。
const cveMatchTaskStatuses = `'created','running','paused'`

// NonTerminalTasksByAssets 给定资产 ID 集，返回每个资产引用的【未终态、未删除】
// 任务 ID（assets.task_ids ↔ tasks.status/deleted_at 联查）。未终态判定走 SQL
// 权威源（不在内存里判终态——内存 task_ids 快照与任务状态可能不同步）。
//   - 确定性：每个资产的任务列表按 task id 升序；
//   - 无关联任务/未知资产的资产不在返回 map 中（零伪造）；
//   - 空/全零输入返回空 map，不发查询。
func (s *AssetStore) NonTerminalTasksByAssets(assetIDs []int64) (map[int64][]int64, error) {
	ids := make([]int64, 0, len(assetIDs))
	seen := make(map[int64]bool, len(assetIDs))
	for _, id := range assetIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[int64][]int64{}, nil
	}
	rows, err := s.db.Query(`
SELECT a.id, t.id
FROM assets a
JOIN tasks t ON t.id = ANY(a.task_ids)
WHERE a.id = ANY($1::bigint[])
  AND t.deleted_at IS NULL
  AND t.status IN (`+cveMatchTaskStatuses+`)
ORDER BY a.id, t.id`, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(ids))
	for rows.Next() {
		var assetID, taskID int64
		if err := rows.Scan(&assetID, &taskID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[assetID] = append(out[assetID], taskID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// B-04E：CVE hint 去重持久层（cve_hints 台账，同一（任务，资产，CVE）只挂一次）
// ---------------------------------------------------------------------------

//go:embed schema_cve_match.sql
var cveMatchSchemaSQL string

// cveMatchSchemaReady 与 bizSchemaReady 同款：每个连接只应用一次 DDL。
var cveMatchSchemaReady sync.Map

// EnsureCVEMatchSchema 应用 cve_hints 的幂等 DDL（schema_cve_match.sql；
// db/schema.sql 是 RESERVED，见该文件头注释）。
func (d *DB) EnsureCVEMatchSchema() error {
	if _, ok := cveMatchSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(cveMatchSchemaSQL); err != nil {
		return fmt.Errorf("应用 CVE hint 去重 schema 失败: %w", err)
	}
	cveMatchSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureCVEMatchSchema 是访问层幂等入口：首次使用补 DDL，之后只是查表。
func (d *DB) ensureCVEMatchSchema() error { return d.EnsureCVEMatchSchema() }

// HasCVEHint 去重快速路径：该（任务，资产，CVE）是否已挂过 hint。重跑同步
// 在发出层先查这里零成本跳过；唯一约束是最终闸。
func (d *DB) HasCVEHint(taskID, assetID int64, cveID string) (bool, error) {
	if taskID <= 0 || assetID <= 0 || strings.TrimSpace(cveID) == "" {
		return false, fmt.Errorf("cve_hints: task/asset id 必须为正且 cve_id 必填")
	}
	if err := d.ensureCVEMatchSchema(); err != nil {
		return false, err
	}
	var exists bool
	err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM cve_hints WHERE task_id=$1 AND asset_id=$2 AND cve_id=$3)`,
		taskID, assetID, cveID).Scan(&exists)
	return exists, err
}

// RecordCVEHint 在 hint 发出成功后落台账。返回 inserted=false 表示该三元组
// 已存在（重跑/并发批次收敛——行保持首见不变，绝不覆盖）。纪律：
//   - verdict 只收 affected/maybe（not_affected 在 B-04C 已剔除，数据层 CHECK
//     再封一道）；
//   - nodeID 必须为正：只有真的发出了 hint 节点才落行——发出失败的批次不留
//     占位，下批重试（绝不因台账占位而丢 hint）。
func (d *DB) RecordCVEHint(taskID, assetID int64, cveID, verdict string, nodeID int64) (bool, error) {
	if taskID <= 0 || assetID <= 0 || strings.TrimSpace(cveID) == "" {
		return false, fmt.Errorf("cve_hints: task/asset id 必须为正且 cve_id 必填")
	}
	switch RangeVerdict(verdict) {
	case VerdictAffected, VerdictMaybe:
	default:
		return false, fmt.Errorf("cve_hints: verdict %q 非法（只收 affected/maybe，not_affected 绝不落台账）", verdict)
	}
	if nodeID <= 0 {
		return false, fmt.Errorf("cve_hints: node_id 必须为正（只在 hint 真实发出后落行）")
	}
	if err := d.ensureCVEMatchSchema(); err != nil {
		return false, err
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO cve_hints(task_id, asset_id, cve_id, verdict, node_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (task_id, asset_id, cve_id) DO NOTHING
RETURNING id`, taskID, assetID, cveID, verdict, nodeID).Scan(&id)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil // 已存在：重跑/并发收敛，首见行保持不变
	}
	return false, err
}
