package agent

// 表格文件（xlsx/csv）里的资产提取（方案 v10 §9 E2-5）——根治「baidu-src.xlsx
// 被当域名入库」：agent 把工作区文件喂给 import_assets_from_file，由这里解析出
// 真实 host/IP，而不是把文件名塞进 domain。零第三方依赖：xlsx 按 zip+xml 直读
// （sharedStrings + 第一个 sheet），csv 用标准库。

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// hostHit 是从表格里提取出的一个候选资产。
type HostHit struct {
	Kind string `json:"kind"` // domain | ip | url
	Raw  string `json:"raw"`
	Host string `json:"host"` // 归一后的登记对象（domain 或 IP；url 取 host）
}

var (
	hostTokenRe = regexp.MustCompile(`(?:https?://[^\s"'<>,;]+)|(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)+)`)
	ipTokenRe   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// 与 db.ValidateAssetDomain 的文件后缀黑名单同口径的粗筛：含常见文件后缀的
	// token 不当域名（精确校验由 db 层闸做）。
	fileishRe = regexp.MustCompile(`(?i)\.(xlsx?|docx?|pptx?|pdf|zip|rar|7z|t?gz|csv|json|xml|ya?ml|txt|md|log|sql|exe|dll|bin|iso|img|apk)([?#].*)?$`)
)

// ParseTabularHosts 解析 xlsx/csv 文件，提取去重后的候选资产（≤500 条）。
func ParseTabularHosts(path string) ([]HostHit, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > 10<<20 {
		return nil, fmt.Errorf("文件超过 10MB 上限")
	}
	var cells []string
	switch {
	case strings.HasSuffix(strings.ToLower(path), ".csv"):
		cells, err = readCSVCells(path)
	default:
		cells, err = readXLSXCells(path)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []HostHit{}
	add := func(kind, raw, host string) {
		host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if host == "" || seen[host] || fileishRe.MatchString(host) {
			return
		}
		if net.ParseIP(host) != nil {
			seen[host] = true
			out = append(out, HostHit{Kind: "ip", Raw: raw, Host: host})
			return
		}
		if strings.Contains(host, ".") && !strings.ContainsAny(host, " \t") {
			seen[host] = true
			out = append(out, HostHit{Kind: kind, Raw: raw, Host: host})
		}
	}
	for _, c := range cells {
		if c == "" {
			continue
		}
		if u, err := url.Parse(strings.TrimSpace(c)); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			add("url", c, u.Hostname())
			continue
		}
		for _, tok := range hostTokenRe.FindAllString(c, -1) {
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				if u, err := url.Parse(tok); err == nil && u.Host != "" {
					add("url", tok, u.Hostname())
				}
				continue
			}
			add("domain", tok, tok)
		}
		for _, ip := range ipTokenRe.FindAllString(c, -1) {
			add("ip", ip, ip)
		}
		if len(out) >= 500 {
			break
		}
	}
	return out, nil
}

// readCSVCells 读全部单元格文本。
func readCSVCells(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r...)
	}
	return out, nil
}

// readXLSXCells 直读 xlsx（zip）：sharedStrings + 第一个 worksheet 的全部单元格。
func readXLSXCells(path string) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 xlsx（zip）文件: %w", err)
	}
	defer zr.Close()
	var shared []string
	var sheetData []byte
	for _, f := range zr.File {
		switch f.Name {
		case "xl/sharedStrings.xml":
			if rc, e := f.Open(); e == nil {
				shared = parseSharedStrings(rc)
				rc.Close()
			}
		case "xl/worksheets/sheet1.xml", "xl/worksheets/sheet.xml":
			if rc, e := f.Open(); e == nil {
				sheetData, _ = io.ReadAll(rc)
				rc.Close()
			}
		}
	}
	if sheetData == nil {
		return nil, fmt.Errorf("xlsx 里没有找到 sheet1.xml")
	}
	return parseSheetCells(sheetData, shared), nil
}

// parseSharedStrings 抽 <si><t>…</t></si> 文本。
func parseSharedStrings(r io.Reader) []string {
	out := []string{}
	dec := xml.NewDecoder(r)
	inSI := false
	var buf strings.Builder
	inT := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI, buf = true, strings.Builder{}
			case "t":
				if inSI {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if inSI {
					out = append(out, buf.String())
					inSI = false
				}
			case "t":
				inT = false
			}
		case xml.CharData:
			if inSI && inT {
				buf.Write(t)
			}
		}
	}
	return out
}

// parseSheetCells 抽每个 <c> 的文本（t="s" 走 sharedStrings，其余取 <v>）。
func parseSheetCells(data []byte, shared []string) []string {
	out := []string{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	inC, inV := false, false
	isShared := false
	var val strings.Builder
	flush := func() {
		if !inC {
			return
		}
		s := strings.TrimSpace(val.String())
		if isShared {
			if i, e := fmtSatoi(s); e == nil && i >= 0 && i < len(shared) {
				s = shared[i]
			} else {
				s = ""
			}
		}
		if s != "" {
			out = append(out, s)
		}
		inC, inV, isShared = false, false, false
		val = strings.Builder{}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "c":
				flush()
				inC = true
				isShared = attrVal(t, "t") == "s"
			case "v":
				if inC {
					inV = true
					val = strings.Builder{}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "c":
				flush()
			case "v":
				inV = false
			}
		case xml.CharData:
			if inC && inV {
				val.Write(t)
			}
		}
	}
	flush()
	return out
}

func attrVal(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// fmtSatoi 是解析 sharedStrings 下标的小工具（避免为它引 strconv 到多处）。
func fmtSatoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
