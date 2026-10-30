// 本文件是策略库（L4）的访问层。
//
// 为什么 L4 是独立顶层图而不是 exploration_nodes 的子表：exploration_nodes 挂
// exploration_id（1:1 UNIQUE + ON DELETE RESTRICT），任务一归档就跟着封存；
// 策略必须跨任务、跨会话存活 → 只能是独立实体。
//
// allow: SIZE_OK —— 本单元的文件域被任务书钉死为三个文件（schema_strategy.sql /
// strategies.go / strategies_test.go），地址识别规则与 CRUD 不得再拆；
// 拆开的收益不足以抵过「写入路径的硬约束被切散」的代价。
package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
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
	var s Strategy
	var activation, steps, pre, tools, crit, fail, params, applies []byte
	if err := row.Scan(&s.ID, &s.CompanyID, &s.Name, &s.Summary,
		&activation, &steps, &pre, &tools, &crit, &fail, &params, &applies,
		&s.State, &s.Confidence, &s.Utility, &s.RunUtility,
		&s.UsageCount, &s.HitCount, &s.MissCount, &s.RetrievalCount,
		&s.Pinned, &s.Version, &s.ValidFrom, &s.InvalidAt, &s.ExpiredAt, &s.RetiredAt,
		&s.RetiredReason, &s.CreatedAt, &s.UpdatedAt); err != nil {
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
