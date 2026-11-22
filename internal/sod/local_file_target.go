package sod

// 本地文件形 target_hint 的判定（0.15.48，0.15.49 判别粒度修正）。
//
// 背景（真实渗透首日实测的成批误拒）：worker 的本地分析命令（python 读下载
// 的 HTML/JSON 做正则扫描）里，正则字面量含 URL 形字符串（如
// `inet=re.compile(r'https?://…')`）⇒ networkIndicatorRe 误判「命令在碰网络」
// ⇒ B5 本地任务目录豁免被反向守卫杀掉、复合动作的 compound_local_segment
// 被跳过 ⇒ 规整器塞进 target_hint 的本地文件名（page__.html/hostmap.py）被
// 当主机名对 scope 裁决 ⇒ 纯本地分析命令成批 out_of_scope 拒绝。
//
// 修法沿既有设计口径（gate.go local_path_target 案例的判断原则——「出网边界
// 由载荷级的 networkIndicatorRe + 快路径 host 提取把守，这里把路径误判成
// host 只会成批误拒本地分析命令」）：把「路径形」扩到「文件形」。
//
// 0.15.49 判别粒度修正（v1 的 networkAPIRe 扫**整载荷**太粗）：混合复合命令
// `cd x && curl … && cat bGet.body` 里 curl 子段把整条载荷染成「有出网能力」
// ⇒ cat 子段的文件形目标被连坐拒绝（实战 #1304/#1318 实锤）。v2 改按
// **子意图自身**三分类裁决：
//   - 网络工具（curl/wget/ssh/…）：目标一律按网络目标照常裁决；
//   - 解释器/间接执行器（python/bash/find/…）：出网能力取决于代码体——
//     内联代码（-c / stdin / heredoc）按**载荷**判（代码体可见）；外部脚本
//     执行（`bash probe.sh`）代码体不可见 ⇒ 保守照常裁决（工作区脚本走
//     B5 绝对路径豁免是设计通道，路径less 形态从严是 B5 同款取舍）；
//   - 纯本地工具（cat/grep/jq/…）：无法出网 ⇒ 文件形目标按构造通过。

import (
	"regexp"
	"strings"
)

// networkAPIRe 匹配【代码体里的真实出网能力】用法（python 网络库 / 抓取工具
// 的调用形态）。与 networkIndicatorRe 的分工：后者匹配 URL 形【字符串】（正则
// 字面量、数据分析里的域名文本都会误中），前者只认出网 API/工具本体。误中只是
// 少一条豁免（宁多审不放松）。
var networkAPIRe = regexp.MustCompile(`(?i)\burlopen\b|urllib|requests\.(?:get|post|put|delete|patch|head|request|Session)|http\.client|httplib|ftplib|smtplib|socket\.(?:socket|create_connection)|aiohttp|httpx|pycurl|mechanize|selenium|\bcurl\b|\bwget\b|\bssh\b|\bscp\b|\bnmap\b|\bdig\b|\bnslookup\b|\bwhois\b`)

// networkToolRe 是【自身即出网】的可执行体：目标一律按网络目标照常 scope
// 裁决，不论 hint 形态（curl 的目标是主机不是文件）。
var networkToolRe = regexp.MustCompile(`(?i)^(?:curl|wget|nc|ncat|netcat|dig|nslookup|whois|host|ssh|scp|sftp|rsync|ftp|telnet|nmap)$`)

// indirectExecRe 是【出网能力取决于代码体】的可执行体：解释器跑代码、
// find/xargs/awk/sed 能间接执行命令。
var indirectExecRe = regexp.MustCompile(`(?i)^(?:python(?:\d+(?:\.\d+)?)?|perl|ruby|node|nodejs|php|lua|Rscript|bash|sh|zsh|dash|ksh|expect|tclsh|find|xargs|awk|sed)$`)

// schemeRe 是 URL 方案形态（带 scheme 的一律网络目标，file:// 也保守按网络
// 走 scope 裁决——worker 用裸路径表示本地文件）。
var schemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.\-]*://`)

// hostShapedRe 是主机名形态（点分标签 + 字母 TLD）。用于区分
// 「example.com/x.py」（URL 路径形态，网络目标）与「data/x.py」（本地路径）。
var hostShapedRe = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// devSocketPrefixes 是 bash 的 /dev/tcp、/dev/udp 网络端点形态——看着是路径，
// 实际是出网（`>/dev/tcp/evil/80`）。路径形豁免必须把它们排除。
var devSocketPrefixes = []string{"/dev/tcp/", "/dev/udp/"}

// fileTargetExts 是「裸名 name.ext 可判定为本地文件引用」的扩展名白名单（小写）。
//
// 拿捏：name.ext 与域名同构（evil.com 与 hostmap.py 形态一样），判「本地」只
// 能靠扩展名知识。白名单外一律按网络目标走 scope 裁决（宁拒不放，方向安全）；
// 白名单取常见产物/脚本/数据/输出件扩展名，覆盖本地分析的主力形态。
var fileTargetExts = map[string]bool{
	"py": true, "pyc": true, "pyo": true, "js": true, "ts": true, "jsx": true, "tsx": true,
	"html": true, "htm": true, "css": true, "scss": true, "json": true, "jsonl": true, "ndjson": true,
	"xml": true, "yml": true, "yaml": true, "toml": true, "ini": true, "cfg": true, "conf": true,
	"properties": true, "env": true, "txt": true, "md": true, "rst": true, "csv": true, "tsv": true,
	"log": true, "out": true, "err": true, "bak": true, "old": true, "tmp": true, "temp": true,
	"dat": true, "bin": true, "db": true, "sqlite": true, "sqlite3": true, "sql": true,
	"sh": true, "bash": true, "zsh": true, "ps1": true, "bat": true, "cmd": true,
	"go": true, "java": true, "rb": true, "php": true, "c": true, "cpp": true, "h": true, "hpp": true,
	"cs": true, "swift": true, "kt": true, "scala": true, "pl": true, "lua": true, "r": true,
	"zip": true, "tar": true, "gz": true, "tgz": true, "bz2": true, "xz": true, "7z": true, "rar": true,
	"jar": true, "war": true, "class": true, "so": true, "dylib": true, "dll": true, "exe": true,
	"key": true, "pem": true, "crt": true, "cer": true, "der": true, "p12": true, "pfx": true, "jks": true,
	"pdf": true, "doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true, "rtf": true,
	"png": true, "jpg": true, "jpeg": true, "gif": true, "bmp": true, "svg": true, "ico": true, "webp": true,
	"mp3": true, "mp4": true, "avi": true, "mov": true, "mkv": true, "wav": true, "ogg": true,
	"woff": true, "woff2": true, "ttf": true, "eot": true, "map": true, "patch": true, "diff": true,
	"pcap": true, "cap": true, "har": true, "torrent": true, "iso": true, "img": true, "ova": true,
	// 0.15.49 实测补：输出件扩展名（curl -o/-D 的产物 bGet.body/hGet.headers
	// 被 v1 白名单漏掉成误拒）。
	"body": true, "headers": true, "hdr": true, "dump": true, "trace": true, "snap": true,
	"pid": true, "lock": true, "part": true, "sig": true, "asc": true, "sha": true, "sum": true,
	"meta": true, "idx": true, "plist": true, "strings": true, "session": true,
}

// localFileTargetHint 判定 target_hint 是否本地文件引用（而非网络主机）。
//
// 逗号列表形态（规整器把多个文件名并进一个 target_hint，0.15.48 实测）：逐段
// 判定，全部文件形才算本地；任何一段像主机 ⇒ 整体按网络目标走 scope 裁决
// （混入主机段的列表保守从严——宁拒不放）。
func localFileTargetHint(hint string) bool {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return false
	}
	for _, part := range strings.Split(hint, ",") {
		if !localFileTargetPart(part) {
			return false
		}
	}
	return true
}

// localFileTargetPart 判定单个目标片段是否本地文件引用。
func localFileTargetPart(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return true // 空段忽略（"a.py," 这类尾逗号）
	}
	// 带 URL scheme 的一律网络目标（file:// 也保守走 scope，worker 用裸路径
	// 表示本地文件）。
	if schemeRe.MatchString(p) {
		return false
	}
	// 路径形：主机名不带路径分隔符；但首段是主机形（example.com/x.py 这种
	// URL 路径形态）或带端口（example.com:8080/x）⇒ 网络目标。/dev/tcp、
	// /dev/udp 是 bash 网络端点，看着是路径实际出网 ⇒ 网络目标。
	if i := strings.IndexAny(p, `/\`); i >= 0 {
		for _, pre := range devSocketPrefixes {
			if strings.HasPrefix(strings.ToLower(p), pre) {
				return false
			}
		}
		first := p[:i]
		if first == "" {
			return true // "/abs/x.py"：以分隔符开头 ⇒ 本地路径
		}
		if strings.Contains(first, ":") {
			return false
		}
		return !hostShapedRe.MatchString(first)
	}
	// 裸名 name.ext：扩展名在白名单内 ⇒ 本地文件引用。无扩展名/尾点/白名单外
	// ⇒ 形态不明或像域名，保守按网络目标裁决。
	dot := strings.LastIndex(p, ".")
	if dot <= 0 || dot == len(p)-1 {
		return false
	}
	return fileTargetExts[strings.ToLower(p[dot+1:])]
}

// inlineCode 判定解释器子意图的代码体是否在载荷里可见（-c 参数 / stdin 形态 /
// heredoc）。外部脚本执行（`bash probe.sh`）代码体不可见——出网能力无从判定，
// 保守照常裁决。
func inlineCode(si StructuredIntent, payload []byte) bool {
	for _, a := range si.Arguments {
		if a == "-c" || a == "-" {
			return true
		}
	}
	return strings.Contains(string(payload), "<<")
}

// scopePassFileTarget 决定规整产物的 target_hint 是否按文件形构造放行。
// 逐条子意图自身裁决（0.15.49 粒度修正），四重前置齐备才放行：
//   - 非快路径（fastPath 提取的 hint 一定是出网主机，必须照常裁决）；
//   - 目标是文件形（localFileTargetHint——主机形目标照常 scope 裁决）；
//   - 可执行体不是网络工具（curl/ssh/… 的目标是主机不是文件）；
//   - 可执行体不是间接执行器，或是但代码体可见且无出网能力（解释器内联
//     载荷过 networkAPIRe；外部脚本执行保守照常裁决）。
//
// 单意图与复合子段共用同一判定（0.15.48：同根因的两处误拒）。
func scopePassFileTarget(si StructuredIntent, payload []byte, fastHit bool) bool {
	if fastHit {
		return false
	}
	if !localFileTargetHint(si.TargetHint) {
		return false
	}
	exec := strings.ToLower(strings.TrimSpace(si.Executable))
	if networkToolRe.MatchString(exec) {
		return false
	}
	if indirectExecRe.MatchString(exec) {
		if !inlineCode(si, payload) {
			return false
		}
		return !networkAPIRe.Match(payload)
	}
	return true
}
