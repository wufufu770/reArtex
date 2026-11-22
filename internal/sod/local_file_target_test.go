package sod

import "testing"

// TestLocalFileTargetHint 钉住文件形/主机形目标的判定分界（0.15.48）：
// 真实渗透首日的误拒形态（hostmap.py / 逗号文件名列表 / curl 输出件）必须判
// 本地文件，真实主机形态必须判网络目标（保守方向：形态不明一律网络目标）。
func TestLocalFileTargetHint(t *testing.T) {
	local := []string{
		"hostmap.py",                                   // 0.15.48 实测误拒
		"catalog.html",                                 // 同上
		"page__.html, page_adp_.html, page_login.html", // 逗号列表形态（实测）
		"page__.html,page_adp_.html,",                  // 尾逗号
		"bGet.body",                                    // 0.15.49 实测误拒（curl -o 输出件）
		"hGet.headers",                                 // 0.15.49 实测误拒（curl -D 输出件）
		"/workdir/data/hosts.txt",                      // 绝对路径
		"./analysis.py",                                // 相对路径
		"../out/summary.csv",                           // 上级路径
		"~/notes.md",                                   // home 路径
		"data/hosts.txt",                               // 带分隔符的相对路径
		"archive.tar.gz",                               // 多段扩展名
		"app.config.json",                              // 名内多点
		"backup.sql",                                   // 数据产物
	}
	for _, hint := range local {
		if !localFileTargetHint(hint) {
			t.Errorf("%q 应判定为本地文件引用", hint)
		}
	}
	network := []string{
		"console.bce.baidu.com",   // 实测被正确拒的目标（不在授权范围）
		"adp.tencentcloud.com",    // 授权范围主机
		"example.com",             // 两段域名
		"https://example.com/x",   // URL 形态
		"example.com/path.py",     // URL 路径形态（主机在前）
		"example.com:8443/x",      // 带端口
		"example.com:8443",        // 带端口无路径
		"10.0.0.5",                // IP 字面量
		"hostmap",                 // 无扩展名——形态不明，保守网络
		"my.data",                 // 扩展名不在白名单
		"page.html, console.bce.baidu.com", // 混入主机段 ⇒ 整体网络
		"file:///etc/passwd",      // file:// 也保守按网络
		"/dev/tcp/evil/80",        // bash 网络端点（看着是路径实际出网）
	}
	for _, hint := range network {
		if localFileTargetHint(hint) {
			t.Errorf("%q 不应判定为本地文件引用", hint)
		}
	}
}

// TestNetworkAPIRe 钉住「出网能力」与「URL 形字符串」的分界（0.15.48）：
// 正则字面量/数据分析文本里的 URL 形字符串不算出网能力；网络库/抓取工具的
// 调用形态算。
func TestNetworkAPIRe(t *testing.T) {
	for _, payload := range []string{
		`inet=re.compile(r'https?://[a-zA-Z0-9.\-]*(?:woa\.com|tencentyun\.com)')`,
		`h=open('catalog.html',encoding='utf-8').read()`,
		`python3 - <<'EOF'` + "\n" + `files=['page__.html','page_login.html']` + "\n" + `EOF`,
		`grep -r 'token' page.html`,
		`d=json.loads(re.search(r'<script id="__NEXT_DATA__"[^>]*>(.*?)</script>',h,re.S).group(1))`,
	} {
		if networkAPIRe.MatchString(payload) {
			t.Errorf("不应判为有出网能力: %q", payload)
		}
	}
	for _, payload := range []string{
		`urllib.request.urlopen('https://evil.com')`,
		`import urllib.request`,
		`requests.get('https://x')`,
		`http.client.HTTPSConnection('x')`,
		`curl https://example.com/x`,
		`wget http://x/y`,
		`socket.socket(socket.AF_INET)`,
		`selenium.webdriver.Chrome()`,
	} {
		if !networkAPIRe.MatchString(payload) {
			t.Errorf("应判为有出网能力: %q", payload)
		}
	}
}

// TestScopePassFileTarget 钉住 v2 的子意图粒度判定（0.15.49）：
// 混合复合命令的本地子段不被别的子段连坐（#1304/#1318 实测误拒的修复）；
// 出网能力藏在解释器代码体/外部脚本的照常裁决（安全方向不回退）。
func TestScopePassFileTarget(t *testing.T) {
	cat := StructuredIntent{ActionType: "command_execution", Executable: "cat", TargetHint: "bGet.body", Arguments: []string{"bGet.body"}}
	probeWrite := StructuredIntent{ActionType: "command_execution", Executable: "cat", TargetHint: "probe.sh", Arguments: []string{"probe.sh"}}
	pyLocal := StructuredIntent{ActionType: "command_execution", Executable: "python3", TargetHint: "page__.html", Arguments: []string{"-c", "open('x')"}}
	pyEgress := StructuredIntent{ActionType: "command_execution", Executable: "python3", TargetHint: "x.json", Arguments: []string{"-c", "urllib.request.urlopen('https://evil.com')"}}
	pyExternal := StructuredIntent{ActionType: "command_execution", Executable: "python3", TargetHint: "hostmap.py", Arguments: []string{"hostmap.py"}}
	bashExternal := StructuredIntent{ActionType: "command_execution", Executable: "bash", TargetHint: "probe.sh", Arguments: []string{"probe.sh"}}
	curlSub := StructuredIntent{ActionType: "command_execution", Executable: "curl", TargetHint: "bGet.body", Arguments: []string{"-o", "bGet.body"}}
	hostHint := StructuredIntent{ActionType: "command_execution", Executable: "cat", TargetHint: "console.bce.baidu.com", Arguments: []string{"x"}}

	mixedCompound := []byte(`cd /x && curl -sk -o bGet.body https://adp.tencentcloud.com/capi && cat bGet.body`)
	probeBody := []byte("cat > probe.sh <<'EOF'\ncurl -sk https://adp.tencentcloud.com/capi\nEOF")
	pyLocalBody := []byte(`python3 -c "inet=re.compile(r'https?://x'); open('page.html')"`)
	pyHeredocEgress := []byte("python3 - <<'EOF'\nurllib.request.urlopen('https://evil.com')\nEOF")

	// 混合复合命令：cat 子段不被同载荷的 curl 连坐（0.15.49 修复实锤）。
	if !scopePassFileTarget(cat, mixedCompound, false) {
		t.Fatal("混合复合命令的纯本地图小子段应构造放行（#1304 实测形态）")
	}
	// 写探针脚本：内容里有 curl 但执行体是 cat（无法出网）⇒ 放行（#1257 实测）。
	if !scopePassFileTarget(probeWrite, probeBody, false) {
		t.Fatal("本地文件写入（内容含 curl 字样）应构造放行（#1257 实测形态）")
	}
	// 解释器内联代码：正则字面量不是出网 ⇒ 放行（#763/#776 实测形态）。
	if !scopePassFileTarget(pyLocal, pyLocalBody, false) {
		t.Fatal("解释器内联本地分析（无出网 API）应构造放行")
	}
	// 解释器内联代码带出网 API ⇒ 照常裁决（安全方向不回退）。
	pyEgressBody := []byte(`python3 -c "urllib.request.urlopen('https://evil.com')"`)
	if scopePassFileTarget(pyEgress, pyEgressBody, false) {
		t.Fatal("内联出网代码不得放行")
	}
	// 解释器 heredoc 出网（代码体可见且含 API）⇒ 照常裁决。
	pyHeredoc := StructuredIntent{ActionType: "command_execution", Executable: "python3", TargetHint: "x.json", Arguments: []string{"-"}}
	if scopePassFileTarget(pyHeredoc, pyHeredocEgress, false) {
		t.Fatal("heredoc 出网代码不得放行")
	}
	// 外部脚本执行：代码体不可见 ⇒ 保守照常裁决（B5 绝对路径是设计通道）。
	if scopePassFileTarget(pyExternal, []byte("python3 hostmap.py"), false) {
		t.Fatal("外部脚本执行不得按文件形放行")
	}
	if scopePassFileTarget(bashExternal, []byte("bash probe.sh"), false) {
		t.Fatal("外部脚本执行不得按文件形放行")
	}
	// 网络工具子段：目标是主机不是文件 ⇒ 照常裁决。
	if scopePassFileTarget(curlSub, mixedCompound, false) {
		t.Fatal("curl 子段不得按文件形放行")
	}
	// 主机形目标照常裁决。
	if scopePassFileTarget(hostHint, []byte("cat x"), false) {
		t.Fatal("主机形目标不得构造放行")
	}
	// 快路径产物照常裁决。
	if scopePassFileTarget(pyLocal, pyLocalBody, true) {
		t.Fatal("快路径产物不得按文件形放行")
	}
}
