// 本文件是策略库（L4）的访问层。
//
// 为什么 L4 是独立顶层图而不是 exploration_nodes 的子表：exploration_nodes 挂
// exploration_id（1:1 UNIQUE + ON DELETE RESTRICT），任务一归档就跟着封存；
// 策略必须跨任务、跨会话存活 → 只能是独立实体。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为三个文件（schema_strategy.sql /
// strategies.go / strategies_test.go），地址识别规则与 CRUD 不得再拆；
// 拆开的收益不足以抵过「写入路径的硬约束被切散」的代价。
// Task 2 的访问层（检索 / 证据链 / 跨层遍历）按任务书追加在同一文件里，
// 理由同上：本单元的写入路径与读路径共用同一套谓词定义，拆文件会让
// 「SQL 谓词」与「Go 侧打分」两份实现漂移。
package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed schema_strategy.sql
var strategySchemaSQL string

// strategySchemaReady 记录某个连接已经应用过策略库 DDL。
//
// 为什么用包级 sync.Map 而不是 DB 上的字段：DB 结构体在 db/db.go 里，本单元不改它。
// 键是底层 *sql.DB 指针 —— 每个测试包/实例各连各的库，互不干扰。
var strategySchemaReady sync.Map

// 策略状态机。quarantined 是 candidate 与 active 之间的**隔离态**：
// 新策略一律先进隔离态，晋级要同时满足三个确定性条件（Task 5）。
const (
	StrategyQuarantined = "quarantined"
	StrategyActive      = "active"
	StrategyRetired     = "retired"
	StrategyTombstone   = "tombstone"
)

// Strategy 是策略库（L4）的一条策略。
//
// 六个 jsonb 列对应市面最成熟的六元组 s=(𝒜, ℐ, ℂ, 𝒯, π, ℰ)：
//
//	Activation       𝒜 激活条件（资产图谓词）
//	Steps            ℐ 步骤（有序数组，每步含工具与参数模板）
//	Preconditions    ℂ 前置条件
//	ToolRequirements 𝒯 工具需求
//	SuccessCriteria  里程碑（命令里程碑 + 阶段里程碑）
//	FailureModes     已知失败模式（从未触发的里程碑自动转此处）
//	Parameters       input_arguments{type,default}
//	AppliesTo        适用范围（资产图谓词的第二组键）
//
// ⚠️ Utility 只是**缓存值**：权威 utility 在 strategy_utility 物化视图。
type Strategy struct {
	ID               int64           `json:"id"`
	CompanyID        *int64          `json:"company_id,omitempty"`
	Name             string          `json:"name"`
	Summary          string          `json:"summary"`
	Activation       json.RawMessage `json:"activation,omitempty"`
	Steps            json.RawMessage `json:"steps,omitempty"`
	Preconditions    json.RawMessage `json:"preconditions,omitempty"`
	ToolRequirements json.RawMessage `json:"tool_requirements,omitempty"`
	SuccessCriteria  json.RawMessage `json:"success_criteria,omitempty"`
	FailureModes     json.RawMessage `json:"failure_modes,omitempty"`
	Parameters       json.RawMessage `json:"parameters,omitempty"`
	AppliesTo        json.RawMessage `json:"applies_to,omitempty"`
	State            string          `json:"state"`
	Confidence       float64         `json:"confidence"`
	Utility          float64         `json:"utility"`
	RunUtility       float64         `json:"run_utility"`
	UsageCount       int64           `json:"usage_count"`
	HitCount         int64           `json:"hit_count"`
	MissCount        int64           `json:"miss_count"`
	RetrievalCount   int64           `json:"retrieval_count"`
	Pinned           bool            `json:"pinned"`
	Version          int64           `json:"version"`
	ValidFrom        time.Time       `json:"valid_from"`
	InvalidAt        *time.Time      `json:"invalid_at,omitempty"`
	ExpiredAt        *time.Time      `json:"expired_at,omitempty"`
	RetiredAt        *time.Time      `json:"retired_at,omitempty"`
	RetiredReason    string          `json:"retired_reason,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// ErrStrategyNotFound 表示策略不存在。
//
// GetStrategy 按接口契约对「不存在」返回 (nil, nil)；本哨兵留给需要把
// 「不存在」与「没有行」区分开的调用方（例如后续的裁决写路径）。
var ErrStrategyNotFound = errors.New("策略不存在")

// EnsureStrategySchema 应用策略库 DDL（幂等）。
//
// db/schema.sql 由别的单元持有，本单元的 DDL 放在 db/schema_strategy.sql，
// 由这里在 db.Open 之后补齐（与 db/chat_group.go 的 EnsureChatGroupSchema 同款）。
// schema 属主把内容并进 db/schema.sql 之后，这里重复应用也无副作用。
//
// ⚠️ **启动 wiring 尚未落地**：本单元不改 db/db.go（该文件由另一个单元并行持有），
// 因此 db.Open 之后**没有**自动调用本函数。首次调用会在建表前补齐 DDL，
// 但「进程启动即建表」这一步属于编排任务，不能漏。
func (d *DB) EnsureStrategySchema() error {
	if _, ok := strategySchemaReady.Load(d.DB); ok {
		return nil
	}
	if err := d.applyStrategySchema(); err != nil {
		return err
	}
	strategySchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureStrategySchema 是访问层的幂等入口：首次调用时建表，之后只是一次 map 查表。
func (d *DB) ensureStrategySchema() error { return d.EnsureStrategySchema() }

// applyStrategySchema 真正执行 DDL（**不查缓存**）—— 幂等性由 SQL 本身保证。
//
// 缓存短路只能证明「同一进程里第二次调用不报错」，证明不了 SQL 幂等；
// 所以留这条不查缓存的路径给测试反复重跑。
func (d *DB) applyStrategySchema() error {
	if _, err := d.Exec(strategySchemaSQL); err != nil {
		return fmt.Errorf("应用策略库 schema 失败: %w", err)
	}
	return nil
}

// CreateStrategy 写入一条策略。
//
// 【新建只允许 quarantined】：晋级是裁决写，只能在宿主侧发生
// （auto-permission-tiers §4.2）。agent 侧的 strategy_propose 走这里，
// 于是它**在结构上**就写不出 active。
//
// 【硬约束：正文不得含真实目标地址】—— 正文里出现形似域名 / IP / URL 的
// 字面量 ⇒ 拒绝写入。详见下面 validateStrategyNoRealTargetAddresses 的注释。
func (d *DB) CreateStrategy(s *Strategy) (int64, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return 0, err
	}
	if s == nil || s.Name == "" {
		return 0, errors.New("策略必须有名字")
	}
	if s.State == "" {
		s.State = StrategyQuarantined
	}
	if s.State != StrategyQuarantined {
		return 0, fmt.Errorf("新建策略只能是 %s（晋级是裁决写，只能在宿主侧发生）", StrategyQuarantined)
	}
	if err := validateStrategyNoRealTargetAddresses(s); err != nil {
		return 0, err
	}
	var id int64
	err := d.QueryRow(`
INSERT INTO strategies(company_id, name, summary, activation, steps, preconditions,
                       tool_requirements, success_criteria, failure_modes, parameters, applies_to, state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		s.CompanyID, s.Name, s.Summary,
		jsonOrEmptyObject(s.Activation), jsonOrEmptyArray(s.Steps),
		jsonOrEmptyArray(s.Preconditions), jsonOrEmptyArray(s.ToolRequirements),
		jsonOrEmptyArray(s.SuccessCriteria), jsonOrEmptyArray(s.FailureModes),
		jsonOrEmptyObject(s.Parameters), jsonOrEmptyArray(s.AppliesTo), s.State).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建策略失败: %w", err)
	}
	s.ID = id
	return id, nil
}

// strategyCols 是 strategies 表的列清单（读取口径的唯一真相）。
const strategyCols = `id, company_id, name, summary, activation, steps, preconditions,
	tool_requirements, success_criteria, failure_modes, parameters, applies_to, state,
	confidence, utility, run_utility, usage_count, hit_count, miss_count, retrieval_count,
	pinned, version, valid_from, invalid_at, expired_at, retired_at, retired_reason,
	created_at, updated_at`

type strategyScanner interface{ Scan(...any) error }

func scanStrategy(row strategyScanner) (*Strategy, error) {
	return scanStrategyWithExtras(row)
}

// GetStrategy 读一条策略。not found 返回 (nil, nil)。
func (d *DB) GetStrategy(id int64) (*Strategy, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	s, err := scanStrategy(d.QueryRow(`SELECT `+strategyCols+` FROM strategies WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读策略失败: %w", err)
	}
	return s, nil
}

// ListStrategies 列出策略。state 为空时列全部；limit ≤ 0 时用默认 200。
//
// 这是 operator 侧的列表接口（agent 侧走 SearchActiveStrategies，只返 active）。
//
// ⚠️ 本函数**不带 company_id 过滤**：租户过滤属于检索路径（Task 2）与
// operator API（Task 6）的口径，Task 1 只提供无租户偏置的原始列表。
// 写入侧仍然按 company_id 隔离（唯一索引 uq_strategies_company_name）。
func (d *DB) ListStrategies(state string, limit int) ([]*Strategy, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if state == "" {
		rows, err = d.Query(`SELECT `+strategyCols+` FROM strategies ORDER BY id DESC LIMIT $1`, limit)
	} else {
		rows, err = d.Query(`SELECT `+strategyCols+` FROM strategies WHERE state=$1 ORDER BY id DESC LIMIT $2`, state, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("列策略失败: %w", err)
	}
	defer rows.Close()
	out := make([]*Strategy, 0, 16)
	for rows.Next() {
		s, err := scanStrategy(rows)
		if err != nil {
			return nil, fmt.Errorf("列策略失败: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列策略失败: %w", err)
	}
	return out, nil
}

// ---------- 正文硬约束：不得含真实目标地址 ----------

// strategyBodyFields 是【正文】字段清单 —— 这些字段只能是参数化模板。
//
// 不含 name / activation / applies_to：名字是检索用的短标题，activation 与
// applies_to 是**资产图谓词**（资产类型 / 服务指纹 / 技术栈标签），按设计本就
// 不含目标地址。
var strategyBodyFields = []struct {
	name string
	text func(*Strategy) string
}{
	{"summary", func(s *Strategy) string { return s.Summary }},
	{"steps", func(s *Strategy) string { return strategyJSONStrings(s.Steps) }},
	{"preconditions", func(s *Strategy) string { return strategyJSONStrings(s.Preconditions) }},
	{"tool_requirements", func(s *Strategy) string { return strategyJSONStrings(s.ToolRequirements) }},
	{"success_criteria", func(s *Strategy) string { return strategyJSONStrings(s.SuccessCriteria) }},
	{"failure_modes", func(s *Strategy) string { return strategyJSONStrings(s.FailureModes) }},
	{"parameters", func(s *Strategy) string { return strategyJSONStrings(s.Parameters) }},
}

// strategyPlaceholderRe 匹配模板占位符：$IDENT 或 ${IDENT}。
var strategyPlaceholderRe = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)

// strategyURLRe 匹配 scheme://authority（authority 到 / ? # 或空白为止）。
var strategyURLRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://([^\s/?#]*)`)

// strategyIPv4Re 匹配点分四段的裸 IPv4 候选。
//
// 为什么正则里没有前后边界断言：Go 的 RE2 **不支持 lookaround**
// （(?<!…)/(?!…) 在编译期就报 invalid named capture，MustCompile 会 panic）。
// 边界判定因此挪到 strategyLooksLikeTargetAddress 里按匹配位置逐字检查 ——
// 语义等价：候选的前后不能紧邻数字或点，否则「1.2.3.4.5」这种版本号会被误判成 IP。
var strategyIPv4Re = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)

// strategyIPv6CandidateRe 找可能是裸 IPv6 的候选串（必须再过 net.ParseIP）。
var strategyIPv6CandidateRe = regexp.MustCompile(`[0-9A-Fa-f:]{2,}`)

// strategyHostRe 匹配形似域名的字面量：至少两段、含点、顶级段是字母。
var strategyHostRe = regexp.MustCompile(`(?i)(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}`)

// strategyHostTLDs 是【会被当成真实主机】的顶级段白名单。
//
// 为什么用白名单而不是「像域名就拒」：正文里大量出现的是**文件与 MIME 类型**
// （steps.json / run.sh / application/vnd.api+json），按「像域名就拒」会把这些
// 全部误杀 ⇒ 策略根本写不进去。白名单只收真实存在的顶级段（公共 TLD + ccTLD）
// 加上常见的**内网保留段**。
//
// ⚠️ 覆盖不到：既不是真实 TLD、也不是内网保留段的生造顶级段（如 target.foo）。
// 但它仍会被 URL 规则命中（只要带 scheme://）⇒ 带协议的真实目标不会漏网。
var strategyHostTLDs = map[string]bool{
	// 公共顶级段
	"com": true, "net": true, "org": true, "edu": true, "gov": true, "mil": true,
	"int": true, "info": true, "biz": true, "name": true, "pro": true, "mobi": true,
	"asia": true, "tel": true, "travel": true, "jobs": true, "land": true,
	"party": true, "download": true, "stream": true, "store": true, "shop": true,
	"site": true, "online": true, "space": true, "website": true, "tech": true,
	"cloud": true, "host": true, "press": true, "life": true, "world": true,
	"today": true, "news": true, "blog": true, "wiki": true, "icu": true,
	"cyou": true, "sbs": true, "cfd": true, "monster": true, "lol": true, "fun": true,
	"fyi": true, "io": true, "ai": true, "app": true, "dev": true, "xyz": true,
	"top": true, "club": true, "design": true, "email": true, "guru": true,
	"ninja": true, "photo": true, "pics": true, "software": true, "solutions": true,
	"systems": true, "tools": true, "digital": true, "network": true, "services": true,
	"agency": true, "media": true, "codes": true, "capital": true, "company": true,
	"works": true, "zone": true, "link": true, "click": true, "one": true,
	"ltd": true, "plus": true, "group": true, "team": true, "center": true,
	"city": true, "global": true, "academy": true, "school": true, "training": true,
	"courses": true, "study": true, "science": true, "energy": true,
	"engineering": true, "market": true, "sale": true, "discount": true,
	"deals": true, "gift": true, "social": true, "chat": true, "forum": true,
	"community": true, "support": true, "help": true, "care": true, "clinic": true,
	"health": true, "fitness": true, "surf": true, "ski": true, "tennis": true,
	"golf": true, "pizza": true, "coffee": true, "pub": true, "cafe": true,
	"restaurant": true, "menu": true, "recipes": true, "kitchen": true, "wine": true,
	"farm": true, "garden": true, "flowers": true, "house": true, "home": true,
	"estate": true, "properties": true, "rentals": true, "realty": true,
	"build": true, "construction": true, "solar": true, "auto": true, "car": true, "cars": true, "bike": true, "boats": true, "flights": true, "cruise": true,
	"holiday": true, "vacations": true, "tours": true, "guide": true, "tips": true,
	"reviews": true, "university": true, "k12": true, "gouv": true,
	// 国家 / 地区顶级段
	"cn": true, "uk": true, "us": true, "de": true, "jp": true, "kr": true,
	"ru": true, "fr": true, "br": true, "in": true, "au": true, "ca": true,
	"nz": true, "it": true, "es": true, "nl": true, "se": true, "no": true,
	"fi": true, "dk": true, "pl": true, "cz": true, "sk": true, "hu": true,
	"ro": true, "bg": true, "gr": true, "pt": true, "be": true, "ch": true,
	"at": true, "ie": true, "il": true, "tr": true, "ua": true, "za": true,
	"ng": true, "ke": true, "eg": true, "ae": true, "sa": true, "ir": true,
	"iq": true, "mx": true, "ar": true, "cl": true, "pe": true, "id": true,
	"my": true, "th": true, "vn": true, "ph": true, "sg": true, "hk": true,
	"tw": true, "mo": true, "pk": true, "bd": true, "np": true,
	"lk": true, "kz": true, "uz": true, "az": true, "ge": true, "am": true,
	"by": true, "md": true, "rs": true, "hr": true, "si": true, "lt": true,
	"lv": true, "ee": true, "is": true, "lu": true, "cy": true, "mt": true,
	// 内网保留段（渗透剧本里极常见，且同样是真实主机）
	"internal": true, "local": true, "lan": true, "corp": true, "intranet": true,
	"test": true, "example": true, "invalid": true, "localhost": true,
	"localdomain": true, "private": true, "domain": true, "arpa": true, "onion": true,
}

// validateStrategyNoRealTargetAddresses 拒绝正文里带真实目标地址的策略。
//
// ⚠️ 这是【结构性约束】，不是【可配置默认值】：
//   - 一个「默认值」可以被配置覆盖（settings / DB / 环境变量 / hot-fix）；
//   - 一个「结构性约束」不能 —— 它写在写入路径上，没有开关。
//
// 本约束因此**不读任何配置**，也不提供任何绕过开关。
//
// 为什么必须拒（不是"提醒"）：
//   - 正文含真实地址 ⇒ 策略退化成「某个目标的攻略」；一旦进了检索注入，
//     它就可能被套到**别的**资产上（把 A 的路径打到 B 的资产上）；
//   - 可复现性由**证据链**保证（strategy_evidence 指向具体的 finding /
//     traffic 快照，带 content_digest），而不是由正文里的硬编码地址保证；
//   - 所以正确的写法是参数化模板（$TARGET / $PATH），实例值走证据链。
//
// 检测面（覆盖正文所有文本字段：summary / steps / preconditions /
// tool_requirements / success_criteria / failure_modes / parameters）：
//   - URL：任意 scheme://host（host 是占位符则放过）；
//   - IPv4：点分四段且每段是合法字节；
//   - IPv6：含 ≥2 个冒号且 net.ParseIP 通过；
//   - 域名：至少两段、含点、顶级段在 strategyHostTLDs 白名单里。
//
// 允许的写法（不误伤）：
//   - "$TARGET/api/v1/users" —— 占位符；
//   - "http://$TARGET/path" —— scheme 在，但 host 是占位符；
//   - "192.168.$HOST:8080" —— 段是占位符而非数字。
//
// 误判怎么办：**fail closed —— 一律拒绝**。宁可让作者把地址挪进证据链，
// 也不让一条带真实地址的策略混进库里（漏网的代价是跨目标复用）。
func validateStrategyNoRealTargetAddresses(s *Strategy) error {
	for _, f := range strategyBodyFields {
		text := f.text(s)
		if text == "" {
			continue
		}
		if hit, ok := strategyLooksLikeTargetAddress(text); ok {
			return fmt.Errorf("策略正文（%s）出现真实目标地址（%s）：正文只能是参数化模板（$TARGET / $PATH），具体实例值请放进证据链（strategy_evidence）", f.name, hit)
		}
	}
	return nil
}

// strategyLooksLikeTargetAddress 返回命中的字面量；第二个返回值为 false 表示没命中。
func strategyLooksLikeTargetAddress(text string) (string, bool) {
	for _, m := range strategyURLRe.FindAllStringSubmatch(text, -1) {
		authority := m[1]
		host := authority
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if strategyPlaceholderRe.MatchString(host) {
			continue
		}
		return m[0], true
	}
	for _, loc := range strategyIPv4Re.FindAllStringIndex(text, -1) {
		if !strategyIPv4BoundaryOK(text, loc[0], loc[1]) {
			continue
		}
		if m := text[loc[0]:loc[1]]; strategyValidIPv4(m) {
			return m, true
		}
	}
	for _, m := range strategyIPv6CandidateRe.FindAllString(text, -1) {
		if strings.Count(m, ":") < 2 {
			continue
		}
		if strategyValidIPv6(m) {
			return m, true
		}
	}
	for _, m := range strategyHostRe.FindAllString(text, -1) {
		tld := m[strings.LastIndex(m, ".")+1:]
		if strategyHostTLDs[strings.ToLower(tld)] {
			return m, true
		}
	}
	return "", false
}

// strategyIPv4BoundaryOK 判断 IPv4 候选的前后是否紧邻数字或点。
//
// 为什么边界必须查：正则表达不了 lookaround（RE2 不支持），而这个边界承载的是
// **误杀防线** —— 不查的话「1.2.3.4.5」「10.0.0.5x」这类串会被当成裸 IP 而
// 拒掉正常正文，策略就写不进去了。
func strategyIPv4BoundaryOK(text string, start, end int) bool {
	if start > 0 {
		if c := text[start-1]; (c >= '0' && c <= '9') || c == '.' {
			return false
		}
	}
	if end < len(text) {
		if c := text[end]; (c >= '0' && c <= '9') || c == '.' {
			return false
		}
	}
	return true
}

// strategyValidIPv4 判断候选是不是合法 IPv4（每段 0..255）。
func strategyValidIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// strategyValidIPv6 判断候选是不是合法 IPv6（允许带 %zone）。
func strategyValidIPv6(s string) bool {
	if i := strings.Index(s, "%"); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	return ip != nil && strings.Contains(s, ":")
}

// strategyJSONStrings 抽出 JSON 字段里的全部字符串值（逐层递归，键不看）。
//
// JSON 非法时退化成扫原始文本（fail closed）：宁可误杀，也不让带地址的正文
// 借「非法 JSON」混过去 —— 非法 JSON 本来也会被 PG 拒掉。
func strategyJSONStrings(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(t)
			sb.WriteByte('\n')
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}

// jsonOrEmptyObject / jsonOrEmptyArray 是 jsonb 列的兜底：空 RawMessage 会让
// PG 报「invalid input syntax for type json」，统一补成合法的空容器。
func jsonOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func jsonOrEmptyArray(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "[]"
	}
	return string(raw)
}

// ---------- Task 2：访问层 —— 有界检索 + 证据链 + 带 hop 上限的跨层遍历 ----------

// 检索参数。全部是【代码常量】—— DB 规则行可被 DELETE、settings 可被热改，
// 与 guard/shortcircuit.go 的同一条理由（单一真相源）。
const (
	// StrategySearchTopK 是硬顶 top-k。反模式 7：top-k 没有阈值就等于把可执行
	// 产物无限量地塞进上下文。
	StrategySearchTopK = 10
	// StrategyCandidatePool 是【单条路径】的候选池上限。
	//
	// ⚠️ 为什么不是「按效用取一条前 N」：按效用单刀切会在字面重排【之前】就丢掉
	// 「结构命中但效用低」的候选 —— 那正是最该被召回的一类。候选池 = 结构路径
	// 子选 ∪ 字面路径子选（两条路径各有自己的上限）。
	StrategyCandidatePool = 50
	// StrategyStructuralPathPool 是【结构路径】子选的上限。
	// 结构命中优先 ⇒ 这一路的子选按匹配强度（谓词覆盖率）排，不按效用排。
	StrategyStructuralPathPool = StrategyCandidatePool
	// StrategyLiteralPathPool 是【字面路径】子选的上限。
	// 这一路按字面相似度排（效用只作次级 tiebreak）。
	StrategyLiteralPathPool = StrategyCandidatePool
	// StrategyCandidatePoolTotal 是两条子选合并后的候选池总上限（硬顶）。
	StrategyCandidatePoolTotal = 100
	// StrategyMinRelevance 是相关性阈值。
	//
	// ⚠️ 它是【字面路径】的阈值：结构命中的候选【不】受它硬过滤
	//    （准入规则 = 结构匹配 OR 字面相似度 ≥ 本阈值）。
	StrategyMinRelevance = 0.20
	// StrategyUtilityWeight 是效用权重 w_u（score = (1-w_u)*relevance + w_u*utility）。
	StrategyUtilityWeight = 0.35
	// StrategyDecayFloor / StrategyDecayCeiling 是有界衰减的上下界。
	//
	// 地板 0.3：衰减只影响排序，【永不过滤候选】（Mem0 口径）—— 衰减不制造
	// 「突然失忆」。
	StrategyDecayFloor   = 0.3
	StrategyDecayCeiling = 1.5
	// StrategyMaxHop 是跨层递归 CTE 的 hop 上限。
	StrategyMaxHop = 4
)

// relevance 的两个贡献项权重。确定性常量，不引入任何模型。
const (
	strategyCoverageWeight = 0.7
	strategyLexicalWeight  = 0.3
)

// strategyDecayRate 是衰减速率：每过一小时乘一次。
const strategyDecayRate = 0.995

// strategyActivationKeys 是 activation 谓词的四键 —— 直接取自现有资产图的列
// （assets.type / service_type / category / record_type），**不另造资产类型体系**
// （设计 §1.1）。
var strategyActivationKeys = []string{"asset_type", "service_type", "category", "record_type"}

// AssetSignature 是任务的资产图签名 —— 策略 activation 的匹配键。
type AssetSignature struct {
	AssetTypes   []string `json:"asset_type"`
	ServiceTypes []string `json:"service_type"`
	Categories   []string `json:"category"`
	RecordTypes  []string `json:"record_type"`
}

// Flattened 返回「资产类型 + 服务类型 + 类别 + 记录类型」的并集（去重排序）。
//
// 用途：applies_to（适用范围 = 资产图谓词的第二组键）的匹配。
func (s AssetSignature) Flattened() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(s.AssetTypes)+len(s.ServiceTypes)+len(s.Categories)+len(s.RecordTypes))
	for _, group := range [][]string{s.AssetTypes, s.ServiceTypes, s.Categories, s.RecordTypes} {
		for _, v := range group {
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// StrategyMatch 是一条候选策略及其打分明细。
type StrategyMatch struct {
	Strategy
	Relevance float64
	Utility   float64
	Decay     float64
	// Score 是最终排序分 = ((1-w_u)*relevance + w_u*utility) * decay。
	Score float64
	// StructuralMatch 标记这条候选是否由【结构路径】捞到（activation 谓词命中）。
	// true ⇒ 准入时**不受**字面阈值硬过滤。
	StructuralMatch bool
	// LexicalSimilarity 是字面路径的 bigram 相似度（0..1）。
	LexicalSimilarity float64
}

// StrategyEvidenceLink 是策略的一条证据引用。
type StrategyEvidenceLink struct {
	RefType       string
	RefID         string
	ContentDigest string
	Role          string
	CreatedAt     time.Time
}

// LayerHop 是跨层遍历的一跳。
//
// Kind 可能是三类：探索节点的 kind（intent / fact / goal …）、LayerHopKindAsset
// （节点锚定的资产，此时 NodeID 是**资产 id**）、LayerHopKindStrategy（L4 侧的策略，
// 此时 StrategyID / StrategyName / StrategyState 有值，NodeID 为 0）。
type LayerHop struct {
	Depth         int
	NodeID        int64
	Kind          string
	StrategyID    int64
	StrategyName  string
	StrategyState string
}

// 跨层遍历里的合成跳类型。
const (
	// LayerHopKindAsset 是资产跳：节点的锚点资产。
	LayerHopKindAsset = "asset"
	// LayerHopKindStrategy 是策略跳：L3 → L4 的跨层引用。
	LayerHopKindStrategy = "strategy"
)

// ---------- 有界检索 ----------

// strategyCoverageSQL 是「谓词覆盖率」的 SQL 表达式。
//
// $1..$4 = 四键的签名取值。覆盖率 = **命中的声明键数 / 资产图键总数（4）**：
// activation 里【声明了】且被签名满足的键各计 1 分；没声明的键不贡献（它不
// 施加任何约束 ⇒ 没有匹配强度可言）。
//
// 为什么分母是 4 而不是「声明键数」：结构路径的 SQL 过滤是合取式的（声明的键
// 全部命中才进池），分母若取声明键数，池内候选的覆盖率恒为 1 ⇒「按匹配强度排」
// 成了摆设，而单键策略的 relevance 也会高到掩盖掉别的候选。
const strategyCoverageSQL = `(
  (CASE WHEN (s.activation ? 'asset_type')   AND (s.activation->'asset_type'   ?| $1::text[]) THEN 1 ELSE 0 END)
+ (CASE WHEN (s.activation ? 'service_type') AND (s.activation->'service_type' ?| $2::text[]) THEN 1 ELSE 0 END)
+ (CASE WHEN (s.activation ? 'category')    AND (s.activation->'category'    ?| $3::text[]) THEN 1 ELSE 0 END)
+ (CASE WHEN (s.activation ? 'record_type') AND (s.activation->'record_type' ?| $4::text[]) THEN 1 ELSE 0 END)
)::float8 / 4.0`

// strategyActivationMatchSQL 是 activation 谓词的**结构过滤**表达式。
//
// 语义：某个键没声明 ⇒ 该键不约束；声明了 ⇒ 它的取值必须与签名有交集。
// 四个键之间是【合取】关系（都满足才命中）。
//
// ⚠️ 这段谓词编译成 SQL WHERE（可直接命中 activation 上的 GIN 索引），**不是**
// 运行时解释 jsonb —— activation 是 SQL 谓词，不是 Go 代码里的 if。
const strategyActivationMatchSQL = `(
  s.activation = '{}'::jsonb
  OR (NOT (s.activation ? 'asset_type')   OR s.activation->'asset_type'   ?| $1::text[])
 AND (NOT (s.activation ? 'service_type') OR s.activation->'service_type' ?| $2::text[])
 AND (NOT (s.activation ? 'category')    OR s.activation->'category'    ?| $3::text[])
 AND (NOT (s.activation ? 'record_type') OR s.activation->'record_type' ?| $4::text[])
)`

// strategyLexicalSQL 是字面路径的 bigram 相似度（Jaccard）。
//
// 参数是**任务文本的 bigram 集合**（Go 侧算好传入）——两边用同一套 bigram 定义
// （字符二元组、去重、集合 Jaccard），因此 SQL 的排序与 Go 侧的打分口径一致。
//
// bigram 的取法与 Go 侧 bigramSet 一致：按字符切、滑动窗口取重叠二元组。
func strategyLexicalSQL(bigramParam string) string {
	inter := fmt.Sprintf("ARRAY(SELECT unnest(lb.bg) INTERSECT SELECT unnest(%s::text[]))", bigramParam)
	return fmt.Sprintf(`COALESCE(
  cardinality(%s)::float8
  / NULLIF(cardinality(lb.bg) + cardinality(%s::text[]) - cardinality(%s), 0),
  0)`, inter, bigramParam, inter)
}

// strategyBigramsLateral 是「策略 name+summary 的 bigram 集合」的 LATERAL 子查询。
//
// 为什么在 SQL 里算 bigram：字面路径要按相似度**取前 N**，排序必须在 SQL 里完成 ——
// 否则就得把整库捞进 Go 侧再排，池子就不再有界。
const strategyBigramsLateral = `LEFT JOIN LATERAL (
  SELECT array_agg(DISTINCT c.chars[g.i] || c.chars[g.i+1]) AS bg
    FROM LATERAL (SELECT regexp_split_to_array(s.name || ' ' || s.summary, '') AS chars) c,
         LATERAL generate_series(1, length(s.name || ' ' || s.summary) - 1) g(i)
) lb ON TRUE`

// strategyArgs 累加查询参数并给出对应的占位符。
type strategyArgs struct{ vals []any }

func (a *strategyArgs) add(v any) string {
	a.vals = append(a.vals, v)
	return fmt.Sprintf("$%d", len(a.vals))
}

// pqTextArray 把 []string 归一成 PG 的 text[] 参数。
//
// ⚠️ nil 必须归一成**空数组**而不是 NULL：`?|` 遇到 NULL 会返回 NULL，
// 于是「签名里没有某键」这件事会从「不匹配」变成「整行被 NULL 吃掉」，
// 结构路径会莫名其妙地漏掉候选。空数组让 `?|` 明确返回 false。
func pqTextArray(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// strategyTenantClause 返回租户过滤子句。
//
// 平台级策略（company_id IS NULL）对所有租户可见；其余策略只对**本任务所属租户**
// 可见。任务没有归属租户（company_id 为空）⇒ 只看平台级策略（fail-closed：
// 宁可少召回，不可跨租户）。
//
// 这是 Task 1 明确留给本层的口径（Task 1 的 ListStrategies 不带租户过滤）。
func strategyTenantClause(args *strategyArgs, companyID int64) string {
	if companyID <= 0 {
		return "(s.company_id IS NULL)"
	}
	return fmt.Sprintf("(s.company_id IS NULL OR s.company_id = %s)", args.add(companyID))
}

// strategyPoolEntry 是候选池里的一条（结构/字面两条子选合并后的中间态）。
type strategyPoolEntry struct {
	strategy     Strategy
	utility      float64
	structural   bool
	lastEvidence time.Time
}

// SearchActiveStrategies 返回与任务资产签名匹配的 active 策略，按最终分排序。
//
// 候选集是【两条独立路径的并集】，不是一条路径：
//
//	① 结构路径（structural）：activation 谓词命中（资产类型 / 服务指纹 /
//	   技术栈标签）—— 捕捉「同一个形状」，与字面重叠无关；
//	② 字面路径（literal）：name+summary 与 goal+description 的字符 bigram
//	   相似度 —— 捕捉「同一批词」。
//
// 两条子选**各有自己的上限**，合并后还有一个总上限；**不做「按效用取前 N」的
// 单刀切** —— 那会在字面重排之前就丢掉「结构命中但效用低」的候选，而那正是最该
// 被召回的一类。
//
// 准入规则是「结构匹配 OR 字面相似度 ≥ 阈值」：一条候选只要结构路径命中，就
// **不得**仅因字面相似度接近 0 而被剔除。
//
// 有界性：结构子选 ≤ StrategyStructuralPathPool、字面子选 ≤
// StrategyLiteralPathPool、合并池 ≤ StrategyCandidatePoolTotal、最终条数 ≤
// limit（且 limit ≤ StrategySearchTopK）。
//
// ⚠️ 两条路径都**不得**引入 LLM 调用 —— 检索热路径上一次模型调用都不许有
// （反模式 4）。结构路径比的是资产图谓词的形状，**不是**语义理解：真正的语义
// 检索（embedding）需要向量存储与重建策略，**留给后续阶段**。
//
// ⚠️ 返回的候选**不携带 steps**：注入上下文只需要摘要，正文按需用 GetStrategy
// 单独取（策略正文是参数化模板，但不该由这条读路径流进上下文）。
func (d *DB) SearchActiveStrategies(taskID int64, sig AssetSignature, limit int, minRelevance float64) ([]StrategyMatch, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > StrategySearchTopK {
		limit = StrategySearchTopK
	}
	if minRelevance <= 0 {
		minRelevance = StrategyMinRelevance
	}
	now := time.Now()

	taskText, err := d.strategyTaskText(taskID)
	if err != nil {
		return nil, err
	}
	companyID, err := d.strategyTaskCompanyID(taskID)
	if err != nil {
		return nil, err
	}

	pool := map[int64]*strategyPoolEntry{}
	if err := d.collectStrategyStructuralPath(pool, sig, companyID); err != nil {
		return nil, err
	}
	if err := d.collectStrategyLiteralPath(pool, sig, companyID, taskText); err != nil {
		return nil, err
	}

	matches := make([]StrategyMatch, 0, len(pool))
	for _, entry := range pool {
		s := entry.strategy
		// utility 的【权威值】在物化视图里，静态列只是缓存 —— 打分用权威值。
		s.Utility = entry.utility
		m := StrategyScore(s, sig, taskText, s.UpdatedAt, entry.lastEvidence, now)
		if entry.structural {
			m.StructuralMatch = true
		}
		if !StrategyQualifies(m, minRelevance) {
			continue
		}
		// 检索结果不携带正文：注入路径只需要摘要。
		m.Steps = nil
		matches = append(matches, m)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) > StrategyCandidatePoolTotal {
		matches = matches[:StrategyCandidatePoolTotal]
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// collectStrategyStructuralPath 收集【结构路径】子选。
//
// 谓词编译成 SQL WHERE（命中 activation 的 GIN 索引），子选按【匹配强度】
// （谓词覆盖率）降序 —— 不按效用：效用低的结构命中候选正是这一步要保住的。
func (d *DB) collectStrategyStructuralPath(pool map[int64]*strategyPoolEntry, sig AssetSignature, companyID int64) error {
	args := &strategyArgs{}
	args.add(pqTextArray(sig.AssetTypes))
	args.add(pqTextArray(sig.ServiceTypes))
	args.add(pqTextArray(sig.Categories))
	args.add(pqTextArray(sig.RecordTypes))
	appliesParam := args.add(pqTextArray(sig.Flattened()))
	tenant := strategyTenantClause(args, companyID)
	limitParam := args.add(StrategyStructuralPathPool)

	q := `SELECT ` + strategyCols + `,
       COALESCE(u.utility_raw, s.utility) AS util_value,
       ` + strategyCoverageSQL + ` AS coverage_value,
       le.last_evidence_at
  FROM strategies s
  LEFT JOIN strategy_utility u ON u.strategy_id = s.id
  LEFT JOIN LATERAL (
       SELECT max(se.created_at) AS last_evidence_at
         FROM strategy_evidence se
        WHERE se.strategy_id = s.id
  ) le ON TRUE
 WHERE s.state = 'active'
   AND s.expired_at IS NULL
   AND ` + tenant + `
   AND ` + strategyActivationMatchSQL + `
   AND (s.applies_to = '[]'::jsonb OR s.applies_to ?| ` + appliesParam + `::text[])
 ORDER BY coverage_value DESC, util_value DESC, s.id
 LIMIT ` + limitParam

	rows, err := d.Query(q, args.vals...)
	if err != nil {
		return fmt.Errorf("读结构路径候选失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var utilValue, coverageValue float64
		var lastEvidence sql.NullTime
		s, err := scanStrategyWithExtras(rows, &utilValue, &coverageValue, &lastEvidence)
		if err != nil {
			return fmt.Errorf("读结构路径候选失败: %w", err)
		}
		pool[s.ID] = &strategyPoolEntry{
			strategy:     *s,
			utility:      utilValue,
			structural:   true,
			lastEvidence: strategyEvidenceTime(lastEvidence, s.CreatedAt),
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读结构路径候选失败: %w", err)
	}
	return nil
}

// collectStrategyLiteralPath 收集【字面路径】子选。
//
// 这一路**不按效用单刀切**，而是按 bigram 相似度取前 N —— 「字面很像但效用低」
// 的策略同样不该在重排前消失。
func (d *DB) collectStrategyLiteralPath(pool map[int64]*strategyPoolEntry, sig AssetSignature, companyID int64, taskText string) error {
	args := &strategyArgs{}
	args.add(pqTextArray(sig.AssetTypes))
	args.add(pqTextArray(sig.ServiceTypes))
	args.add(pqTextArray(sig.Categories))
	args.add(pqTextArray(sig.RecordTypes))
	bigramParam := args.add(pqTextArray(bigramList(taskText)))
	tenant := strategyTenantClause(args, companyID)
	limitParam := args.add(StrategyLiteralPathPool)

	q := `SELECT ` + strategyCols + `,
       COALESCE(u.utility_raw, s.utility) AS util_value,
       ` + strategyCoverageSQL + ` AS coverage_value,
       ` + strategyLexicalSQL(bigramParam) + ` AS lexical_value,
       le.last_evidence_at
  FROM strategies s
  LEFT JOIN strategy_utility u ON u.strategy_id = s.id
  LEFT JOIN LATERAL (
       SELECT max(se.created_at) AS last_evidence_at
         FROM strategy_evidence se
        WHERE se.strategy_id = s.id
  ) le ON TRUE
  ` + strategyBigramsLateral + `
 WHERE s.state = 'active'
   AND s.expired_at IS NULL
   AND ` + tenant + `
 ORDER BY lexical_value DESC, util_value DESC, s.id
 LIMIT ` + limitParam

	rows, err := d.Query(q, args.vals...)
	if err != nil {
		return fmt.Errorf("读字面路径候选失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var utilValue, coverageValue, lexicalValue float64
		var lastEvidence sql.NullTime
		s, err := scanStrategyWithExtras(rows, &utilValue, &coverageValue, &lexicalValue, &lastEvidence)
		if err != nil {
			return fmt.Errorf("读字面路径候选失败: %w", err)
		}
		entry, ok := pool[s.ID]
		if !ok {
			entry = &strategyPoolEntry{strategy: *s}
			pool[s.ID] = entry
		}
		entry.utility = utilValue
		if entry.lastEvidence.IsZero() {
			entry.lastEvidence = strategyEvidenceTime(lastEvidence, s.CreatedAt)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读字面路径候选失败: %w", err)
	}
	return nil
}

// strategyTaskText 读任务的 goal + description —— 字面路径的比较基准。
func (d *DB) strategyTaskText(taskID int64) (string, error) {
	if taskID <= 0 {
		return "", nil
	}
	var goal, description string
	err := d.QueryRow(`SELECT goal, description FROM tasks WHERE id=$1`, taskID).Scan(&goal, &description)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读任务文本失败: %w", err)
	}
	return goal + " " + description, nil
}

// strategyTaskCompanyID 读任务的归属租户（0 = 无租户）。
func (d *DB) strategyTaskCompanyID(taskID int64) (int64, error) {
	if taskID <= 0 {
		return 0, nil
	}
	var companyID sql.NullInt64
	err := d.QueryRow(`SELECT company_id FROM tasks WHERE id=$1`, taskID).Scan(&companyID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读任务租户失败: %w", err)
	}
	return companyID.Int64, nil
}

// strategyEvidenceTime 决定「上次证据更新」的时间：没有证据链时退回创建时间。
//
// 退回创建时间而不是零值：一条刚建、还没挂证据的策略不该一出生就吃满衰减。
func strategyEvidenceTime(lastEvidence sql.NullTime, createdAt time.Time) time.Time {
	if lastEvidence.Valid {
		return lastEvidence.Time
	}
	return createdAt
}

// StrategyScore 算出一条候选策略的最终分。
//
// score(s) = (1 - w_u) * relevance(s, task) + w_u * utility(s)
// final(s) = score(s) * decay(s)
//
// relevance 是**确定性**的：0.7 * 谓词覆盖率 + 0.3 * 字面相似度（字符 bigram）。
// 不用语义模型 —— 检索要可复现、可审计、可在无 LLM 的路径上运行。
//
// ⚠️ 「谓词覆盖率」就是**结构路径**的贡献项 —— 它由 activation 谓词命中多少个
// 键算出来，与字面相似度无关 ⇒ 一条结构命中、字面相似度 ≈ 0 的策略仍然拿得到
// 非零 relevance。
//
// ⚠️ **准入判定不是「relevance >= minRelevance」**：准入是
//
//	structural_match OR 字面相似度 >= minRelevance
//
// —— 相关性阈值**不对结构命中的候选做硬过滤**（否则「同一个漏洞类、措辞完全
// 不同」的策略会被漏掉）。阈值仍然有效，只是作用在字面路径上。
func StrategyScore(s Strategy, sig AssetSignature, taskText string, lastUse, lastEvidence, now time.Time) StrategyMatch {
	m := StrategyMatch{Strategy: s, Utility: s.Utility}
	coverage := strategyActivationCoverage(s.Activation, sig)
	lexical := bigramOverlap(strategyTextOf(s), taskText)
	m.Relevance = strategyCoverageWeight*coverage + strategyLexicalWeight*lexical
	m.LexicalSimilarity = lexical
	m.Decay = StrategyDecay(lastUse, lastEvidence, now)
	score := (1-StrategyUtilityWeight)*m.Relevance + StrategyUtilityWeight*m.Utility
	m.Score = score * m.Decay
	m.StructuralMatch = strategyActivationMatches(s.Activation, sig)
	return m
}

// StrategyQualifies 判定一条候选是否够格进入结果集。
//
// 规则：**结构匹配 OR 字面相似度 ≥ minRelevance**。
//
// 为什么不是「只看 relevance」：relevance 把结构与字面加权成一个标量，
// 一个「结构强命中 + 字面为零」的候选会被标量阈值误杀。准入判定必须
// **显式区分两条路径**，否则结构路径等于白加。
func StrategyQualifies(m StrategyMatch, minRelevance float64) bool {
	if m.StructuralMatch {
		return true
	}
	return m.LexicalSimilarity >= minRelevance
}

// StrategyDecay 算有界衰减因子。
//
//	recency = 0.995^(距上次使用小时数)
//	fresh   = 0.995^(距上次证据更新小时数)
//	boost   = 1 + 0.5*(1-fresh)
//	decay   = clamp(recency * boost, 0.3, 1.5)
//
// ⚠️ decay **只影响排序，永不过滤**：地板 0.3 保证一条老策略不会突然消失。
func StrategyDecay(lastUse, lastEvidence, now time.Time) float64 {
	recency := math.Pow(strategyDecayRate, strategyHoursSince(lastUse, now))
	fresh := math.Pow(strategyDecayRate, strategyHoursSince(lastEvidence, now))
	boost := 1 + 0.5*(1-fresh)
	decay := recency * boost
	if decay < StrategyDecayFloor {
		return StrategyDecayFloor
	}
	if decay > StrategyDecayCeiling {
		return StrategyDecayCeiling
	}
	return decay
}

// strategyHoursSince 算两个时间点之间的小时数（负数按 0 算）。
func strategyHoursSince(t, now time.Time) float64 {
	if t.IsZero() {
		return math.Inf(1)
	}
	h := now.Sub(t).Hours()
	if h < 0 {
		return 0
	}
	return h
}

// strategyTextOf 取策略的字面文本（name + summary）。
//
// steps / preconditions 等正文**不参与**字面匹配：正文是参数化模板，拿它去比
// 任务的措辞只会制造噪声。
func strategyTextOf(s Strategy) string {
	return s.Name + " " + s.Summary
}

// bigramOverlap 是两个文本的字符 bigram Jaccard 相似度。
//
// 为什么用 bigram 而不是分词：中文没有空白分隔，按空白切词会把一句话切成一坨；
// bigram 对中文与 ASCII 都能给出稳定的确定性相似度，不需要任何词典。
//
// ⚠️ 这只是**字面路径**。单靠它会漏掉「同一个漏洞类、但措辞完全不同」的策略 ——
// 实证上接近一半的高度相关命中落在 Jaccard < 0.05 区间，且字面重叠与相关性的
// 相关性只有 r ≈ −0.29 ⇒ 必须有结构路径兜底。
func bigramOverlap(a, b string) float64 {
	sa := bigramSet(a)
	sb := bigramSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for g := range sa {
		if _, ok := sb[g]; ok {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// bigramSet 取文本的字符 bigram 集合（去重）。
func bigramSet(text string) map[string]struct{} {
	runes := []rune(text)
	if len(runes) < 2 {
		return map[string]struct{}{}
	}
	seen := make(map[string]struct{}, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		seen[string(runes[i:i+2])] = struct{}{}
	}
	return seen
}

// bigramList 取文本的字符 bigram 集合（排序后的切片）—— 给 SQL 的 text[] 参数用。
//
// 排序是为了让同一个文本每次生成的参数完全一致（可复现的查询指纹）。
func bigramList(text string) []string {
	set := bigramSet(text)
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// strategyActivationParse 把 activation 解析成「键 → 取值集合」。
//
// 一个键的取值可能是数组（正常形状）或字符串（单值）。非数组非字符串的取值
// （数字 / 对象 / null）⇒ 声明了但不可满足。
func strategyActivationParse(activation json.RawMessage) map[string][]string {
	out := map[string][]string{}
	if len(activation) == 0 {
		return out
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(activation, &raw); err != nil {
		return out
	}
	for key, value := range raw {
		var arr []string
		if err := json.Unmarshal(value, &arr); err == nil {
			out[key] = arr
			continue
		}
		var one string
		if err := json.Unmarshal(value, &one); err == nil {
			out[key] = []string{one}
			continue
		}
		out[key] = nil
	}
	return out
}

// strategyActivationMatches 判断 activation 谓词是否被签名满足。
//
// 与 strategyActivationMatchSQL（SQL 侧的结构过滤）同形：没声明的键不约束，
// 声明了的键必须有交集。
func strategyActivationMatches(activation json.RawMessage, sig AssetSignature) bool {
	values := strategyActivationParse(activation)
	for _, key := range strategyActivationKeys {
		vals, declared := values[key]
		if !declared {
			continue
		}
		if len(vals) == 0 {
			return false
		}
		if !strategyValueSetsIntersect(vals, strategySignatureValues(key, sig)) {
			return false
		}
	}
	return true
}

// strategyActivationCoverage 算「谓词覆盖率」：命中的声明键数 / 资产图键总数。
//
// 与 strategyCoverageSQL（SQL 侧）同形 —— 两处必须一致，否则排序与打分会对不上。
func strategyActivationCoverage(activation json.RawMessage, sig AssetSignature) float64 {
	values := strategyActivationParse(activation)
	matched := 0
	for _, key := range strategyActivationKeys {
		vals, declared := values[key]
		if !declared || len(vals) == 0 {
			continue
		}
		if strategyValueSetsIntersect(vals, strategySignatureValues(key, sig)) {
			matched++
		}
	}
	return float64(matched) / float64(len(strategyActivationKeys))
}

// strategySignatureValues 取签名里某个 activation 键对应的取值。
func strategySignatureValues(key string, sig AssetSignature) []string {
	switch key {
	case "asset_type":
		return sig.AssetTypes
	case "service_type":
		return sig.ServiceTypes
	case "category":
		return sig.Categories
	case "record_type":
		return sig.RecordTypes
	}
	return nil
}

// strategyValueSetsIntersect 判断两个取值集合是否有交集。
func strategyValueSetsIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

// ---------- 证据链 ----------

// strategyEvidenceRefTypes 是允许的证据引用类型（与 strategy_evidence 的 CHECK 一致）。
var strategyEvidenceRefTypes = map[string]bool{
	"finding":          true,
	"traffic_snapshot": true,
	"activity":         true,
	"tool_call":        true,
	"run":              true,
}

// strategyEvidenceRoles 是允许的证据角色（与 strategy_evidence 的 CHECK 一致）。
var strategyEvidenceRoles = map[string]bool{
	"created_by":      true,
	"validated_by":    true,
	"contradicted_by": true,
}

// LinkStrategyEvidence 记一条证据引用。
//
// 引用完整性由**写入时校验**：调用方给的 ref_id 必须真实存在 —— 没有级联外键
// （任务归档不得连带删掉策略证据链），一条凭空捏造的引用会让策略变成「没有出处
// 的断言」，也就是「凭空提权」。
//
// ⚠️ ref_id 是 **TEXT**（不是 BIGINT）：traffic_evidence_snapshots.id 是 TEXT 主键，
// BIGINT 装不下。
func (d *DB) LinkStrategyEvidence(strategyID int64, refType, refID, role, contentDigest string) error {
	if err := d.ensureStrategySchema(); err != nil {
		return err
	}
	if strategyID <= 0 {
		return errors.New("策略 id 不合法")
	}
	if !strategyEvidenceRefTypes[refType] {
		return fmt.Errorf("证据引用类型 %q 不合法", refType)
	}
	if !strategyEvidenceRoles[role] {
		return fmt.Errorf("证据角色 %q 不合法", role)
	}
	if strings.TrimSpace(refID) == "" {
		return errors.New("证据引用 id 不能为空")
	}
	if err := d.validateStrategyEvidenceRef(refType, refID); err != nil {
		return err
	}
	_, err := d.Exec(`
INSERT INTO strategy_evidence(strategy_id, ref_type, ref_id, content_digest, role)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT (strategy_id, ref_type, ref_id, role) DO UPDATE SET content_digest = EXCLUDED.content_digest`,
		strategyID, refType, refID, contentDigest, role)
	if err != nil {
		return fmt.Errorf("记策略证据失败: %w", err)
	}
	return nil
}

// validateStrategyEvidenceRef 校验被引用的实体真实存在。
//
// 只对**有实体表**的引用类型生效：finding → findings、traffic_snapshot →
// traffic_evidence_snapshots。其余类型（activity / tool_call / run）尚无统一的
// 实体表，引用完整性由各自的写入方保证。
func (d *DB) validateStrategyEvidenceRef(refType, refID string) error {
	var (
		table string
		cast  string
	)
	switch refType {
	case "finding":
		table, cast = "findings", "bigint"
	case "traffic_snapshot":
		table, cast = "traffic_evidence_snapshots", "text"
	default:
		return nil
	}
	var exists bool
	err := d.QueryRow(fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1::%s)`, table, cast), refID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("校验证据引用失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("证据引用不存在：%s#%s", refType, refID)
	}
	return nil
}

// StrategyEvidence 列出策略的证据链。
func (d *DB) StrategyEvidence(strategyID int64) ([]StrategyEvidenceLink, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
SELECT ref_type, ref_id, content_digest, role, created_at
  FROM strategy_evidence
 WHERE strategy_id = $1
 ORDER BY created_at, ref_type, ref_id, role`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("读策略证据失败: %w", err)
	}
	defer rows.Close()
	out := []StrategyEvidenceLink{}
	for rows.Next() {
		var link StrategyEvidenceLink
		if err := rows.Scan(&link.RefType, &link.RefID, &link.ContentDigest, &link.Role, &link.CreatedAt); err != nil {
			return nil, fmt.Errorf("读策略证据失败: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略证据失败: %w", err)
	}
	return out, nil
}

// ---------- 带 hop 上限的跨层遍历 ----------

// TraverseFromFinding 从一条 finding 出发，跨 L3→L4 遍历，带硬 hop 上限。
//
// 方向：**沿入边回溯**（dst → src）。理由：finding 是探索图的叶子，它的
// 【出处】在入边上（intent --yields--> finding、fact --derived_from--> intent），
// 回溯才走得回「这条漏洞是怎么来的」。
//
// 递归 CTE 的 `WHERE l.depth < N` **同时**防两类问题：环形图的无限循环，
// 以及分支因子爆炸导致的退化（资产图一个 root_domain 下的 subdomain 数
// 很容易超过 30）。
//
// 跳的种类：探索节点（intent / fact / goal …）、节点锚定的资产
// （LayerHopKindAsset）、以及引用了该 finding 的策略（LayerHopKindStrategy）。
func (d *DB) TraverseFromFinding(findingID int64, maxDepth int) ([]LayerHop, error) {
	if err := d.ensureStrategySchema(); err != nil {
		return nil, err
	}
	if maxDepth <= 0 || maxDepth > StrategyMaxHop {
		maxDepth = StrategyMaxHop
	}

	rows, err := d.Query(`
WITH RECURSIVE layer(depth, node_id, kind, exploration_id) AS (
    SELECT 0, f.node_id, n.kind, n.exploration_id
      FROM findings f
      JOIN exploration_nodes n ON n.id = f.node_id
     WHERE f.id = $1 AND f.node_id IS NOT NULL
    UNION ALL
    SELECT l.depth + 1, e.src_id, n.kind, l.exploration_id
      FROM layer l
      JOIN exploration_edges e
        ON e.dst_id = l.node_id
       AND e.exploration_id = l.exploration_id
      JOIN exploration_nodes n ON n.id = e.src_id
     WHERE l.depth < $2
)
SELECT l.depth, l.node_id, l.kind
  FROM layer l
 WHERE l.depth <= $2
 ORDER BY l.depth, l.node_id`, findingID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("跨层遍历失败: %w", err)
	}
	hops := []LayerHop{}
	nodeDepth := map[int64]int{}
	for rows.Next() {
		var hop LayerHop
		if err := rows.Scan(&hop.Depth, &hop.NodeID, &hop.Kind); err != nil {
			rows.Close()
			return nil, fmt.Errorf("跨层遍历失败: %w", err)
		}
		nodeDepth[hop.NodeID] = hop.Depth
		hops = append(hops, hop)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("跨层遍历失败: %w", err)
	}
	rows.Close()

	assetHops, err := d.strategyAssetHops(nodeDepth, maxDepth)
	if err != nil {
		return nil, err
	}
	hops = append(hops, assetHops...)

	strategyHops, err := d.strategyLinkedHops(findingID)
	if err != nil {
		return nil, err
	}
	hops = append(hops, strategyHops...)

	sort.SliceStable(hops, func(i, j int) bool {
		if hops[i].Depth != hops[j].Depth {
			return hops[i].Depth < hops[j].Depth
		}
		if hops[i].Kind != hops[j].Kind {
			return hops[i].Kind < hops[j].Kind
		}
		return hops[i].NodeID < hops[j].NodeID
	})
	return hops, nil
}

// strategyAssetHops 返回被访问节点的锚点资产（资产跳）。
//
// 资产是节点的叶子：走 node → asset 算一跳，所以资产跳的深度 = 节点深度 + 1。
func (d *DB) strategyAssetHops(nodeDepth map[int64]int, maxDepth int) ([]LayerHop, error) {
	if len(nodeDepth) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(nodeDepth))
	for id := range nodeDepth {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows, err := d.Query(`
SELECT node_id, asset_id
  FROM exploration_anchors
 WHERE node_id = ANY($1::bigint[])
 ORDER BY node_id, asset_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("读节点锚点失败: %w", err)
	}
	defer rows.Close()
	hops := []LayerHop{}
	for rows.Next() {
		var nodeID, assetID int64
		if err := rows.Scan(&nodeID, &assetID); err != nil {
			return nil, fmt.Errorf("读节点锚点失败: %w", err)
		}
		depth := nodeDepth[nodeID] + 1
		if depth > maxDepth {
			continue
		}
		hops = append(hops, LayerHop{Depth: depth, NodeID: assetID, Kind: LayerHopKindAsset})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读节点锚点失败: %w", err)
	}
	return hops, nil
}

// strategyLinkedHops 返回引用了该 finding 的策略（策略跳）。
//
// 两条证据路径都算数：finding 直接作为证据引用，以及经由 traffic 快照
// （finding_traffic_bindings → strategy_evidence 的 traffic_snapshot 引用）。
func (d *DB) strategyLinkedHops(findingID int64) ([]LayerHop, error) {
	rows, err := d.Query(`
SELECT DISTINCT s.id, s.name, s.state
  FROM strategy_evidence se
  JOIN strategies s ON s.id = se.strategy_id
 WHERE (se.ref_type = 'finding' AND se.ref_id = $2::text)
    OR (se.ref_type = 'traffic_snapshot' AND se.ref_id IN (
          SELECT fb.snapshot_id FROM finding_traffic_bindings fb WHERE fb.finding_id = $1))`,
		findingID, fmt.Sprint(findingID))
	if err != nil {
		return nil, fmt.Errorf("读策略引用失败: %w", err)
	}
	defer rows.Close()
	hops := []LayerHop{}
	for rows.Next() {
		var hop LayerHop
		if err := rows.Scan(&hop.StrategyID, &hop.StrategyName, &hop.StrategyState); err != nil {
			return nil, fmt.Errorf("读策略引用失败: %w", err)
		}
		hop.Depth = 1
		hop.Kind = LayerHopKindStrategy
		hops = append(hops, hop)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读策略引用失败: %w", err)
	}
	return hops, nil
}

// ---------- 任务的资产签名 ----------

// TaskAssetSignature 算出任务【范围内】资产的资产图签名。
//
// ⚠️ 必须走 scope 注入版查询（ScopeFragmentFor）：直接查 assets 全表会把范围外
// 资产带进签名，策略匹配就成了一条范围外的旁路（Phase 0 的读面隔离白做）。
//
// 这是【安全前提】，不是性能优化：签名决定哪些策略会被激活，范围外的资产类型
// 进了签名 ⇒ 一条为别的目标写的策略会被套到本任务上。
//
// 签名只取资产图已有的四个键，不另造类型体系。
func (s *AssetStore) TaskAssetSignature(taskID int64) (AssetSignature, error) {
	fragment, err := s.ScopeFragmentFor(taskID)
	if err != nil {
		return AssetSignature{}, fmt.Errorf("取任务范围失败: %w", err)
	}
	// 聚合结果取 jsonb 而不是 text[]：pgx 的 database/sql 驱动没有数组的 Scan
	// 目标（与 db/skill_usage.go 记的同一条理由），jsonb 走 []byte 再解析最省事。
	var assetTypes, serviceTypes, categories, recordTypes []byte
	row := s.db.QueryRow(`WITH `+fragment.CTE+`
SELECT COALESCE(jsonb_agg(DISTINCT a.type)         FILTER (WHERE a.type         <> ''), '[]'::jsonb),
       COALESCE(jsonb_agg(DISTINCT a.service_type) FILTER (WHERE a.service_type <> ''), '[]'::jsonb),
       COALESCE(jsonb_agg(DISTINCT a.category)     FILTER (WHERE a.category     <> ''), '[]'::jsonb),
       COALESCE(jsonb_agg(DISTINCT a.record_type)  FILTER (WHERE a.record_type  <> ''), '[]'::jsonb)
  FROM assets a
 WHERE `+fragment.Predicate, fragment.Args...)
	if err := row.Scan(&assetTypes, &serviceTypes, &categories, &recordTypes); err != nil {
		return AssetSignature{}, fmt.Errorf("算任务资产签名失败: %w", err)
	}
	sig := AssetSignature{}
	for _, pair := range []struct {
		raw  []byte
		dest *[]string
	}{
		{assetTypes, &sig.AssetTypes},
		{serviceTypes, &sig.ServiceTypes},
		{categories, &sig.Categories},
		{recordTypes, &sig.RecordTypes},
	} {
		if err := json.Unmarshal(pair.raw, pair.dest); err != nil {
			return AssetSignature{}, fmt.Errorf("解析任务资产签名失败: %w", err)
		}
		sort.Strings(*pair.dest)
	}
	return sig, nil
}

// ---------- 读取口径的公共扫描 ----------

// scanStrategyWithExtras 读一行策略，并追加扫描调用方给的额外列。
//
// strategyCols 的顺序是读取口径的唯一真相；额外的列排在策略列之后。
func scanStrategyWithExtras(row strategyScanner, extras ...any) (*Strategy, error) {
	var s Strategy
	var activation, steps, pre, tools, crit, fail, params, applies []byte
	dests := []any{&s.ID, &s.CompanyID, &s.Name, &s.Summary,
		&activation, &steps, &pre, &tools, &crit, &fail, &params, &applies,
		&s.State, &s.Confidence, &s.Utility, &s.RunUtility,
		&s.UsageCount, &s.HitCount, &s.MissCount, &s.RetrievalCount,
		&s.Pinned, &s.Version, &s.ValidFrom, &s.InvalidAt, &s.ExpiredAt, &s.RetiredAt,
		&s.RetiredReason, &s.CreatedAt, &s.UpdatedAt}
	dests = append(dests, extras...)
	if err := row.Scan(dests...); err != nil {
		return nil, err
	}
	s.Activation = json.RawMessage(activation)
	s.Steps = json.RawMessage(steps)
	s.Preconditions = json.RawMessage(pre)
	s.ToolRequirements = json.RawMessage(tools)
	s.SuccessCriteria = json.RawMessage(crit)
	s.FailureModes = json.RawMessage(fail)
	s.Parameters = json.RawMessage(params)
	s.AppliesTo = json.RawMessage(applies)
	return &s, nil
}
