package sod

// 方案 v10 §14 R1 —— 确定性快路径（能力恢复）。
//
// 【豁免的是 LLM 规整，不是闸门裁决】：命中快路径的调用跳过规整器（不再花一次
// 模型调用去把一条形态上确定只读的命令/请求转成结构化意图），由本文件用纯代码
// 给出结构化意图；闸门的五道检查（scope → tool_health → quota → tier2 →
// anomaly）**照跑** —— scope 用本文件提取的 TargetHint 判定（与 LLM 规整结果
// 走同一个入口，见 gate.go 检查 ①），其余四道读实时状态，一道不少。审计带
// fast_path 独立标记（chain.go 的 markers，§4 不变式 4：所有放行旁路带独立
// 审计标记）。
//
// 判定纪律：【纯代码、白名单、保守方向】——
//   - 只有形态完全确定的安全只读出网才命中：curl 纯 GET/HEAD 单目标拉取、
//     nmap -sn/-p（无 --script）、dig / nslookup / whois / host 被动查询、
//     WebFetch；
//   - 任何拿不准的形态（未知旗、复合命令、命令替换、变量展开、多目标、含
//     写/破坏词）一律【不命中】⇒ 照旧走 LLM 规整。不命中**不是放行** ——
//     只是「这次省不了那次模型调用」，宁可多花一次调用，不放行一个没判定的
//     形态；
//   - 本文件只回答「规整器要不要省」：它不是闸门、不做裁决、不读实时状态
//     （额度 / 熔断 / 范围行都归 chain 与 gate）。
//
// 本文件同时向 R2 模式缓存提供确定性的目标/方法提取（fastPathTargetHint /
// fastPathMethod）：模式键 = tool|host|method，host 即这里的提取结果 ——
// 键里的 host 与 scope 判定的 hint 出自同一函数 ⇒ 模式缓存复用的意图所指向
// 的目标，与本次载荷词法上指向的目标恒等。

import (
	"encoding/json"
	"net"
	"regexp"
	"strings"
)

// fastPathDenyChars 是快路径的一票否决字符：shell 复合 / 重定向 / 替换 / 展开
// 指示符。`>` `<` 重定向（写文件 / 吃文件）；`|` 管道；`;` `&` 复合命令（后半段
// 没被判定过）；反引号与 `$` 命令替换 / 变量展开（命令面被藏起来，scope 判的
// host 可能不是真正会连的 host）；换行是多命令；`!` 历史扩展。URL 查询串里的
// `&`、带 `$` 的口令会被误伤 —— 那类命令只是【不命中】快路径，照旧走 LLM
// 规整，方向保守。
const fastPathDenyChars = "><|;&`$\n\r!"

// fastPathDenyWordRe 是写/破坏词的词边界否决面（方案 §14 R1：含 tee / rm /
// mv / chmod 等写/破坏词不算）。词边界防止误伤普通子串；即便误伤到 URL 路径
// 段（如 /cp/），后果也只是该条不命中快路径，方向保守。
var fastPathDenyWordRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])(?:tee|rm|mv|cp|ln|chmod|chown|chattr|dd|mkfs|shred|truncate|wget|nc|ncat|netcat|socat|sudo|doas|eval|exec|xargs|awk|sed|perl|python2?|python3|ruby|php|bash|sh|dash|zsh|fish|ksh|powershell|base64)(?:$|[^a-z0-9_])`)

// fastPathDenied 报告命令是否含复合 / 替换 / 展开指示符或写破坏词（一票否决）。
func fastPathDenied(cmd string) bool {
	if strings.ContainsAny(cmd, fastPathDenyChars) {
		return true
	}
	return fastPathDenyWordRe.MatchString(cmd)
}

// DeterministicFastPath 判定一次调用能否走确定性快路径（方案 v10 §14 R1）。
//
// 命中 ⇒ 返回纯代码给出的结构化意图（ActionType + TargetHint；AnomalyFlag 恒
// false —— 快路径不产出异常信号），第二个返回值为 true；未命中 ⇒ 零值 +
// false，调用方照旧走 LLM 规整。两种形态：
//   - Bash：command 命中安全只读出网模式（curl 纯 GET/HEAD / nmap -sn 或 -p
//     且无 --script / dig / nslookup / whois / host），且能提取出唯一的
//     域名/IP 目标；
//   - WebFetch：url 字段的 host 是可判定的域名/IP。
//
// 之外的一切（其它工具、其它形态、解不动的载荷）一律未命中。
func DeterministicFastPath(toolName string, payload []byte) (NormalizerResult, bool) {
	var res NormalizerResult
	if len(payload) == 0 {
		return res, false
	}
	var (
		host       string
		actionType string
	)
	switch toolName {
	case "Bash":
		cmd, ok := bashCommandField(payload)
		if !ok || fastPathDenied(cmd) {
			return res, false
		}
		host, ok = bashFastPathTarget(cmd)
		if !ok {
			return res, false
		}
		actionType = actionTypeCommandExecution
	case "WebFetch":
		host = extractURLHost(jsonStringField(payload, "url"))
		if !validFastPathHost(host) {
			return res, false
		}
		actionType = actionTypeHTTPRequest
	default:
		return res, false
	}
	res.StructuredIntent.ActionType = actionType
	res.StructuredIntent.TargetHint = host
	return res, true
}

// fastPathTargetHint 从载荷里做确定性目标提取（方案 v10 §14 R2 模式键的 host
// 分量）。纯词法、不做安全判定：URL host 优先；命令形态（curl / nmap /
// dig / nslookup / whois / host）取第一个可判定的域名/IP。提取不到 ⇒ ""
// （无模式键）。口径与快路径的 TargetHint 一致（小写、剥端口、剥尾点）。
func fastPathTargetHint(toolName string, payload []byte) string {
	switch toolName {
	case "Bash":
		cmd := jsonStringField(payload, "command")
		if strings.TrimSpace(cmd) == "" {
			return ""
		}
		if m := urlRe.FindStringSubmatch(cmd); m != nil {
			if h := normalizeFastPathHost(m[1]); validFastPathHost(h) {
				return h
			}
		}
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return ""
		}
		switch fields[0] {
		case "curl":
			for _, tok := range fields[1:] {
				if strings.HasPrefix(tok, "-") {
					continue
				}
				if h := curlURLHost(stripWrappingQuotes(tok)); h != "" {
					return h
				}
			}
		case "nmap", "dig", "nslookup", "whois", "host":
			for _, tok := range fields[1:] {
				if strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "@") || strings.HasPrefix(tok, "+") {
					continue
				}
				if h := normalizeFastPathHost(stripWrappingQuotes(tok)); validFastPathHost(h) {
					return h
				}
			}
		}
		return ""
	case "WebFetch":
		h := extractURLHost(jsonStringField(payload, "url"))
		if validFastPathHost(h) {
			return h
		}
		return ""
	default:
		return ""
	}
}

// fastPathMethod 提取载荷的确定性「方法」分量（方案 v10 §14 R2 模式键）：
// 结构化 HTTP 工具读 payload 的 method 字段；Bash 从命令面读 -X/--request
// （含 -I/--head ⇒ HEAD），其余 ⇒ ""（curl 不带方法旗即默认 GET 形态）。
func fastPathMethod(toolName string, payload []byte) string {
	switch toolName {
	case "Bash":
		cmd := jsonStringField(payload, "command")
		fields := strings.Fields(cmd)
		for i := 1; i < len(fields); i++ {
			tok := fields[i]
			switch {
			case tok == "-X" || tok == "--request":
				if i+1 < len(fields) {
					return methodWord(fields[i+1])
				}
				return ""
			case strings.HasPrefix(tok, "-X") && len(tok) > 2:
				return methodWord(tok[2:])
			case strings.HasPrefix(tok, "--request="):
				return methodWord(strings.TrimPrefix(tok, "--request="))
			case tok == "-I" || tok == "--head":
				return "HEAD"
			}
		}
		return ""
	case "WebFetch":
		return strings.ToUpper(strings.TrimSpace(jsonStringField(payload, "method")))
	default:
		return ""
	}
}

// methodWord 把方法词归一成大写字母串；形态不明 ⇒ ""。
func methodWord(s string) string {
	s = strings.ToUpper(strings.TrimSpace(stripWrappingQuotes(s)))
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return s
}

// ---- 基础提取 ----

// bashCommandField 从 Bash 载荷里取 command 字段（严格 JSON：解不出 ⇒ 无快路径）。
func bashCommandField(payload []byte) (string, bool) {
	s := jsonStringField(payload, "command")
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// jsonStringField 从 JSON 载荷顶层取一个字符串字段；缺字段 / 解不动 ⇒ ""。
func jsonStringField(payload []byte, field string) string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	raw, ok := probe[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// stripWrappingQuotes 剥掉成对包裹的引号（shell 把引号留给了 curl 的参数形态
// —— 引号本身不改变参数语义）。不配对 ⇒ 原样返回（后续形态判定会拒）。
func stripWrappingQuotes(tok string) string {
	if len(tok) >= 2 && (tok[0] == '"' || tok[0] == '\'') && tok[len(tok)-1] == tok[0] {
		return tok[1 : len(tok)-1]
	}
	return tok
}

// extractURLHost 从 URL 里提取 host。只认 http/https —— curl 还能讲 ftp /
// telnet / gopher 等几十种协议，快路径只放形态完全确定的两种。取不出可判定的
// host ⇒ ""。口径与 db.normalizeScopeHost 一致：剥 scheme / userinfo / path /
// 端口 / 尾点 / 大小写。
func extractURLHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	var rest string
	switch {
	case strings.HasPrefix(lower, "http://"):
		rest = trimmed[len("http://"):]
	case strings.HasPrefix(lower, "https://"):
		rest = trimmed[len("https://"):]
	default:
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	return normalizeFastPathHost(rest)
}

// normalizeFastPathHost 归一化 host：剥端口（IPv6 先剥方括号）、转小写、剥尾点。
func normalizeFastPathHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// fastPathLabelRe 是域名单个 label 的形态（不含点；中间允许 - / _）。
var fastPathLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)

// validFastPathHost 报告 host 是「域名或 IP」（方案 §14 R1 的 TargetHint 形态）。
// IP：net.ParseIP 认；域名：≥2 个合法 label 且末段（TLD）全字母 ≥2。单标签
// 主机名（intranet 等）不是可判定的目标形态 ⇒ 不算（保守：交给 LLM 规整路径）。
func validFastPathHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for i, label := range labels {
		if i == len(labels)-1 {
			tld := strings.ToLower(label)
			if !isAllLetters(tld) || len(tld) < 2 {
				return false
			}
			continue
		}
		if !fastPathLabelRe.MatchString(strings.ToLower(label)) {
			return false
		}
	}
	return true
}

func isAllLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// ---- Bash 命令形态判定 ----

// bashFastPathTarget 判定命令形态并提取唯一目标 host。安全形态之外的命令一律
// ("", false)。
func bashFastPathTarget(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", false
	}
	switch fields[0] {
	case "curl":
		return curlFastPathTarget(fields)
	case "nmap":
		return nmapFastPathTarget(fields)
	case "dig":
		return digFastPathTarget(fields)
	case "nslookup":
		return nslookupFastPathTarget(fields)
	case "whois":
		return whoisFastPathTarget(fields)
	case "host":
		return hostFastPathTarget(fields)
	default:
		return "", false
	}
}

// safeCurlShortChars 是 curl 单连字符短旗的允许字符集（可组合，如 -sI）。
// 其余任何字符 ⇒ 整条不命中。注意 -X / -H / -A / -e / -u / -m 这些【带值】
// 短旗不进本集合 —— 单独判定。
const safeCurlShortChars = "sSkIivf46q#"

// safeCurlLongFlags 是 curl 无值长旗白名单。白名单外一律不命中（默认拒绝：
// --data / --form / -o / --output / -K / --proxy / -L / --resolve …… 全都
// 不在表里 —— 带 body、写文件、改代理、跟随重定向、DNS 重绑定的形态都进不了
// 快路径）。
var safeCurlLongFlags = map[string]bool{
	"--silent": true, "--show-error": true, "--head": true, "--insecure": true,
	"--verbose": true, "--fail": true, "--compressed": true, "--path-as-is": true,
	"--ipv4": true, "--ipv6": true, "--get": true, "--no-keepalive": true,
	"--http1.0": true, "--http1.1": true, "--http2": true, "--http2-prior-knowledge": true,
}

// valueCurlLongFlags 是 curl 带一个值的长旗白名单。
var valueCurlLongFlags = map[string]bool{
	"--max-time": true, "--connect-timeout": true, "--retry": true, "--url": true,
	"--header": true, "--user-agent": true, "--referer": true, "--user": true,
	"--request": true, "--noproxy": true,
}

// safeCurlShortToken 报告短旗 bundle 的每个字符都在允许集内（-sI、-sS 等）。
func safeCurlShortToken(tok string) bool {
	for _, ch := range tok[1:] {
		if !strings.ContainsRune(safeCurlShortChars, ch) {
			return false
		}
	}
	return true
}

// curlFastPathTarget 判定 curl 命令是否是「纯 GET/HEAD 单目标拉取」并提取
// URL host。规则（全部白名单，默认拒绝）：
//   - 短旗字符必须在允许集内；长旗必须在无值 / 带值白名单内；
//   - -X/--request 只接受 GET；不带方法旗且没有带 body 的旗 ⇒ 默认 GET/HEAD
//     形态（-I/--head 是 HEAD）；
//   - -H 的值不得以 @ 开头（@file 从文件读 header，形态不明）；
//   - 有且只有一个 URL（显式 http/https 或裸 host）；多 URL ⇒ 不命中
//     （scope 只判一个 host，多目标让 LLM 规整路径去拆）。
func curlFastPathTarget(fields []string) (string, bool) {
	method := ""
	head := false
	url := ""
	urlCount := 0
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		if strings.HasPrefix(tok, "--") {
			name, value, hasValue := tok, "", false
			if j := strings.IndexByte(tok, '='); j >= 0 {
				name, value, hasValue = tok[:j], tok[j+1:], true
			}
			switch {
			case safeCurlLongFlags[name]:
				if name == "--head" {
					head = true
				}
			case valueCurlLongFlags[name]:
				if !hasValue {
					if i+1 >= len(fields) {
						return "", false
					}
					i++
					value = fields[i]
				}
				switch name {
				case "--url":
					if urlCount > 0 {
						return "", false
					}
					url = value
					urlCount++
				case "--request":
					method = methodWord(value)
				case "--header":
					if strings.HasPrefix(value, "@") {
						return "", false
					}
				}
			default:
				return "", false
			}
			continue
		}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			switch {
			case tok == "-X":
				if i+1 >= len(fields) {
					return "", false
				}
				i++
				method = methodWord(fields[i])
			case strings.HasPrefix(tok, "-X"): // -XGET 贴值形态
				method = methodWord(tok[2:])
			case tok == "-I":
				head = true
			case tok == "-H" || tok == "-A" || tok == "-e" || tok == "-u" || tok == "-m":
				if i+1 >= len(fields) {
					return "", false
				}
				if tok == "-H" && strings.HasPrefix(fields[i+1], "@") {
					return "", false
				}
				i++
			default:
				if !safeCurlShortToken(tok) {
					return "", false
				}
				if strings.ContainsRune(tok, 'I') { // bundle 里的 -I（如 -sI）
					head = true
				}
			}
			continue
		}
		// 非 flag token：curl 把每个非旗参数都当 URL；多 URL ⇒ 不命中。
		if urlCount > 0 {
			return "", false
		}
		url = stripWrappingQuotes(tok)
		urlCount++
	}
	if urlCount != 1 {
		return "", false
	}
	if method != "" && method != "GET" {
		return "", false
	}
	if head && method != "" { // -I 与 -X 同给：HEAD/GET 冲突形态，不判定
		return "", false
	}
	host := curlURLHost(url)
	if host == "" {
		return "", false
	}
	return host, true
}

// curlURLHost 提取 curl 目标参数的 host：显式 http/https URL，或裸 host 形态
// （curl 对无 scheme 的目标默认按 http:// 处理）。其余协议形态（ftp:// 等）
// ⇒ ""。
func curlURLHost(raw string) string {
	h := extractURLHost(raw)
	if h == "" && !strings.Contains(raw, "://") {
		// 裸 host[:port][/path] 形态
		host := raw
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		h = normalizeFastPathHost(host)
	}
	if !validFastPathHost(h) {
		return ""
	}
	return h
}

// safeNmapFlags 是 nmap 的无值旗白名单。白名单外一律不命中（默认拒绝：
// --script / -A / -iL / -oN / -oX / --datadir / --resume / -S / -D …… ——
// 脚本引擎能写文件、 decoy/欺骗形态目标不明，方案 §14 R1 明令排除 --script）。
var safeNmapFlags = map[string]bool{
	"-sn": true, "-sT": true, "-sS": true, "-sU": true, "-sV": true,
	"-Pn": true, "-n": true, "-F": true, "-v": true, "-4": true, "-6": true,
	"--open": true, "--reason": true,
}

// nmapFastPathTarget 判定 nmap 命令是否是「-sn 或 -p 的单目标发现 / 端口扫描」
// 并提取目标。多目标 / CIDR ⇒ 不命中（scope 只判一个 host，交给 LLM 规整路径）。
func nmapFastPathTarget(fields []string) (string, bool) {
	matched := false // 命中 -sn 或 -p（方案只点名这两类形态）
	target := ""
	targetCount := 0
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		switch {
		case tok == "-p" || tok == "-T" || tok == "--max-rate" || tok == "--min-rate":
			matched = matched || tok == "-p"
			if i+1 >= len(fields) {
				return "", false
			}
			i++
		case strings.HasPrefix(tok, "-p") && len(tok) > 2: // -p22,80 贴值形态
			matched = true
		case len(tok) == 3 && strings.HasPrefix(tok, "-T") && tok[2] >= '1' && tok[2] <= '5':
			// -T4 速率档
		case safeNmapFlags[tok]:
			matched = matched || tok == "-sn"
		default:
			if strings.HasPrefix(tok, "-") {
				return "", false // 未知旗（--script 等）一票否决
			}
			target = stripWrappingQuotes(tok)
			targetCount++
		}
	}
	if !matched || targetCount != 1 {
		return "", false
	}
	host := normalizeFastPathHost(target)
	if !validFastPathHost(host) {
		return "", false
	}
	return host, true
}

// dnsTypeClassWords 是 dig 位置参数里除被查名字外允许出现的 RR type / class
// 词（大小写不敏感）。
var dnsTypeClassWords = map[string]bool{
	"A": true, "AAAA": true, "ANY": true, "AXFR": true, "IXFR": true,
	"CNAME": true, "HINFO": true, "MX": true, "NS": true, "PTR": true,
	"SOA": true, "TXT": true, "SRV": true, "NAPTR": true, "DNSKEY": true,
	"DS": true, "RRSIG": true, "NSEC": true, "NSEC3": true, "NSEC3PARAM": true,
	"TLSA": true, "SSHFP": true, "CAA": true, "SVCB": true, "HTTPS": true,
	"LOC": true, "SPF": true, "DNAME": true, "CERT": true, "KX": true,
	"IN": true, "CH": true, "HS": true, "NONE": true,
}

// dnsHint 按分类器（classifier.go）的同一世界观取 hint：查询必经的公共 DNS/
// whois 基础设施（公共解析器 / whois.iana.org）不是范围目标 —— 指了基础设施
// 时 hint 用被查的名字；指了非基础设施的 server（如在测目标自己的 NS）时
// hint 用 server 本身。server 形态不明 ⇒ 不命中。
func dnsHint(name, server string) (string, bool) {
	if server != "" {
		h := normalizeFastPathHost(stripWrappingQuotes(server))
		if !validFastPathHost(h) {
			return "", false
		}
		if !intelInfraHost(h) {
			return h, true
		}
	}
	if validFastPathHost(name) {
		return name, true
	}
	return "", false
}

// digFastPathTarget 判定 dig 被动查询并提取目标。规则：
//   - 旗只认 -4/-6/-t/-c/-p/-y/-q/-x（其余默认拒绝：-f 批量文件、-k 密钥文件
//     等形态不明）；+ 开头的运行选项全放（dig 的 +option 都是只读查询修饰）；
//   - 位置参数：第一个是被查名字，其后只认 RR type/class 词；名字可由 -q/-x
//     给出（-x 的值即反查 IP）。
func digFastPathTarget(fields []string) (string, bool) {
	name, server := "", ""
	typeWords := 0
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		switch {
		case strings.HasPrefix(tok, "@"):
			if server != "" {
				return "", false
			}
			server = tok[1:]
		case strings.HasPrefix(tok, "+"):
			// +option：查询修饰（+short/+tcp/+noall …），只读形态
		case tok == "-4" || tok == "-6":
		case tok == "-t" || tok == "-c" || tok == "-p" || tok == "-y":
			if i+1 >= len(fields) {
				return "", false
			}
			i++
		case tok == "-q" || tok == "-x":
			if i+1 >= len(fields) || name != "" {
				return "", false
			}
			i++
			name = stripWrappingQuotes(fields[i])
		case strings.HasPrefix(tok, "-"):
			return "", false // 未知旗
		default:
			t := stripWrappingQuotes(tok)
			if name == "" {
				name = t
				continue
			}
			if !dnsTypeClassWords[strings.ToUpper(t)] {
				return "", false
			}
			typeWords++
			if typeWords > 2 { // 名字 + type + class 之外的位置参数形态不明
				return "", false
			}
		}
	}
	if name == "" {
		return "", false
	}
	return dnsHint(name, server)
}

// nslookupValueFlagPrefixes 是 nslookup「-选项=值」形态的前缀（查询选项）。
var nslookupValueFlagPrefixes = []string{"-type=", "-querytype=", "-qt=", "-class=", "-cl=", "-port=", "-timeout=", "-retry="}

// safeNslookupFlags 是 nslookup 无值旗白名单。
var safeNslookupFlags = map[string]bool{
	"-debug": true, "-d2": true, "-nodebug": true, "-norecurse": true,
	"-recurse": true, "-vc": true, "-novc": true, "-fail": true, "-nofail": true,
	"-ignoretc": true, "-noignoretc": true, "-search": true, "-nosearch": true,
}

// nslookupFastPathTarget 判定 nslookup 被动查询并提取目标。位置参数 name
// [server]；无名字 = 进交互模式，形态不明 ⇒ 不命中。
func nslookupFastPathTarget(fields []string) (string, bool) {
	name, server := "", ""
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		if safeNslookupFlags[tok] || hasAnyPrefix(tok, nslookupValueFlagPrefixes) {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			return "", false // 未知旗
		}
		t := stripWrappingQuotes(tok)
		switch {
		case name == "":
			name = t
		case server == "":
			server = t
		default:
			return "", false // 第三个位置参数形态不明
		}
	}
	if name == "" {
		return "", false
	}
	return dnsHint(name, server)
}

// whoisFastPathTarget 判定 whois 被动查询并提取目标。旗只认 -h server /
// -p port / -H / -V（其余 whois 客户端旗各家实现不一，形态不明）；位置参数
// 有且只有一个（被查名字）。
func whoisFastPathTarget(fields []string) (string, bool) {
	name, server := "", ""
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		switch tok {
		case "-h":
			if i+1 >= len(fields) || server != "" {
				return "", false
			}
			i++
			server = fields[i]
		case "-p":
			if i+1 >= len(fields) {
				return "", false
			}
			i++
		case "-H", "-V":
		default:
			if strings.HasPrefix(tok, "-") {
				return "", false // 未知旗
			}
			if name != "" {
				return "", false
			}
			name = stripWrappingQuotes(tok)
		}
	}
	if name == "" {
		return "", false
	}
	return dnsHint(name, server)
}

// hostFastPathTarget 判定 host 被动查询并提取目标。旗只认 -a/-4/-6/-T/-v/-w
// 与带值的 -W/-R/-t/-c；位置参数 name [server]。
func hostFastPathTarget(fields []string) (string, bool) {
	name, server := "", ""
	for i := 1; i < len(fields); i++ {
		tok := fields[i]
		switch {
		case tok == "-a" || tok == "-4" || tok == "-6" || tok == "-T" || tok == "-v" || tok == "-w":
		case tok == "-W" || tok == "-R" || tok == "-t" || tok == "-c":
			if i+1 >= len(fields) {
				return "", false
			}
			i++
		case len(tok) > 2 && (strings.HasPrefix(tok, "-t") || strings.HasPrefix(tok, "-c") ||
			strings.HasPrefix(tok, "-W") || strings.HasPrefix(tok, "-R")):
			// 贴值形态 -tMX / -W5
		case strings.HasPrefix(tok, "-"):
			return "", false // 未知旗
		default:
			t := stripWrappingQuotes(tok)
			switch {
			case name == "":
				name = t
			case server == "":
				server = t
			default:
				return "", false
			}
		}
	}
	if name == "" {
		return "", false
	}
	return dnsHint(name, server)
}

// hasAnyPrefix 报告 s 是否以 prefixes 中任一为前缀。
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
