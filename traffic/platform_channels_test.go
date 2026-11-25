package traffic

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustParseChannels 是平台通道白名单测试的帮手：解析失败直接炸出调用栈。
func mustParseChannels(t *testing.T, raw string) *platformChannelSet {
	t.Helper()
	set, err := parsePlatformChannels([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestParsePlatformChannels pins the parse contract：归一化（小写、去空白）、
// 空主机剔除、空名字拒绝（宁可启动失败也不装一条没有身份的通道）。
func TestParsePlatformChannels(t *testing.T) {
	set := mustParseChannels(t, `[
		{"name":"  Enrich ","hosts":[" .Assets.Example.com ","api.example.com",""]},
		{"name":"enrich","hosts":[".override.example.com"]},
		{"name":"browser-mcp","hosts":[]}
	]`)

	// 重名以后者为准（不静默合并，让 operator 自己看配置）。
	ch := set.byName["enrich"]
	if len(ch.Hosts) != 1 || ch.Hosts[0] != ".override.example.com" {
		t.Fatalf("重名条目应以后者为准，实际：%v", ch.Hosts)
	}
	if ch2 := set.byName["browser-mcp"]; len(ch2.Hosts) != 0 {
		t.Fatalf("空主机表应保持为空（登记了也不放行），实际：%v", ch2.Hosts)
	}
	if _, ok := set.byName["  Enrich "]; ok {
		t.Fatal("通道名必须归一化，不得保留原始大小写/空白键")
	}

	if _, err := parsePlatformChannels([]byte(`[{"name":"","hosts":["x"]}]`)); err == nil {
		t.Fatal("name 为空的条目必须拒绝解析")
	}
	if _, err := parsePlatformChannels([]byte(`not-json`)); err == nil {
		t.Fatal("非 JSON 必须拒绝解析")
	}
}

// TestLoadPlatformChannels pins the loader's fail-closed direction：路径为空或
// 文件不存在 = 空白名单（全拒，不回落放行）；读失败/解析失败 = 空白名单 + 错误
// 交还调用方打日志。
func TestLoadPlatformChannels(t *testing.T) {
	// 路径为空。
	set, err := LoadPlatformChannels("")
	if err != nil {
		t.Fatalf("空路径应得到空白名单而非错误: %v", err)
	}
	if set == nil || len(set.byName) != 0 {
		t.Fatal("空路径必须得到空白名单")
	}

	// 文件不存在。
	set, err = LoadPlatformChannels(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("文件不存在应得到空白名单而非错误: %v", err)
	}
	if len(set.byName) != 0 {
		t.Fatal("文件不存在必须得到空白名单")
	}

	// 解析失败：空白名单 + 错误。
	set, err = LoadPlatformChannels(writeTempFile(t, "broken", `{"not":"a-list"`))
	if err == nil {
		t.Fatal("解析失败必须把错误交还调用方（供启动日志）")
	}
	if len(set.byName) != 0 {
		t.Fatal("解析失败必须按空白名单处理（fail-closed）")
	}

	// 读失败（路径是个目录）：空白名单 + 错误。
	set, err = LoadPlatformChannels(t.TempDir())
	if err == nil {
		t.Fatal("读失败必须把错误交还调用方")
	}
	if len(set.byName) != 0 {
		t.Fatal("读失败必须按空白名单处理")
	}

	// 正常装载。
	set, err = LoadPlatformChannels(writeTempFile(t, "channels.json",
		`[{"name":"enrich","hosts":[".assets.example.com"]}]`))
	if err != nil {
		t.Fatalf("合法白名单必须装载成功: %v", err)
	}
	ch, ok := set.byName["enrich"]
	if !ok || !matchPlatformHost("a.assets.example.com", ch.Hosts[0]) {
		t.Fatalf("装载结果不对: %+v", ch)
	}
}

// TestSetPlatformChannelsNilBecomesEmpty pins that a nil injection is the empty
// whitelist，不是「关闭校验」——nil 语义只能是「全拒」。
func TestSetPlatformChannelsNilBecomesEmpty(t *testing.T) {
	tr := &Traffic{}
	tr.SetPlatformChannels(nil)
	req, _ := http.NewRequest(http.MethodGet, "http://a.assets.example.com/x", nil)
	req.Header.Set(platformChannelHeader, "enrich")
	if _, ok := tr.platformChannelAllowed(req); ok {
		t.Fatal("nil 白名单必须等价空白名单：不得放行")
	}
}

// TestMatchPlatformHost pins the host-matching contract：后缀条目（前导点）命中
// 自身与任意子域，但不命中「看着像」的其他域；非点条目只做精确比对。
func TestMatchPlatformHost(t *testing.T) {
	cases := []struct {
		host, entry string
		want        bool
	}{
		{"a.assets.example.com", ".assets.example.com", true},
		{"assets.example.com", ".assets.example.com", true},     // 后缀条目含自身
		{"notassets.example.com", ".assets.example.com", false}, // 前导点必须整段匹配
		{"a.assets.example.com.evil.com", ".assets.example.com", false},
		{"api.example.com", "api.example.com", true},
		{"other.example.com", "api.example.com", false},
		{"api.example.com", "example.com", false}, // 精确条目（无前导点）不做后缀匹配
	}
	for _, c := range cases {
		if got := matchPlatformHost(c.host, c.entry); got != c.want {
			t.Fatalf("matchPlatformHost(%q, %q) = %v，期望 %v", c.host, c.entry, got, c.want)
		}
	}
}

// TestPlatformChannelHeaderIsConstant pins the header name：它是平台客户端与
// 判定层之间的契约，改名等于静默拆掉所有通道声明。
func TestPlatformChannelHeaderIsConstant(t *testing.T) {
	if platformChannelHeader != "X-Artex-Channel" {
		t.Fatalf("通道头名被改动：%q（A-01 契约，改动必须同步所有平台客户端）", platformChannelHeader)
	}
	if EnvPlatformChannels == "" || !strings.HasPrefix(EnvPlatformChannels, "ARTEX_") {
		t.Fatalf("环境变量名不符合 ARTEX_ 前缀纪律：%q", EnvPlatformChannels)
	}
}

// writeTempFile 把内容写进临时目录并返回路径（白名单装载测试用）。
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
