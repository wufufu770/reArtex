package sod

import "testing"

// TestLocalFileTargetHint 钉住文件形/主机形目标的判定分界（0.15.48）：
// 真实渗透首日的误拒形态（hostmap.py / 逗号文件名列表）必须判本地文件，
// 真实主机形态必须判网络目标（保守方向：形态不明一律网络目标）。
func TestLocalFileTargetHint(t *testing.T) {
	local := []string{
		"hostmap.py",                                              // 0.15.48 实测误拒
		"catalog.html",                                            // 同上
		"page__.html, page_adp_.html, page_login.html",            // 逗号列表形态（实测）
		"page__.html,page_adp_.html,",                             // 尾逗号
		"/workdir/data/hosts.txt",                                 // 绝对路径
		"./analysis.py",                                           // 相对路径
		"../out/summary.csv",                                      // 上级路径
		"~/notes.md",                                              // home 路径
		"data/hosts.txt",                                          // 带分隔符的相对路径
		"archive.tar.gz",                                          // 多段扩展名
		"app.config.json",                                         // 名内多点
		"backup.sql",                                              // 数据产物
	}
	for _, hint := range local {
		if !localFileTargetHint(hint) {
			t.Errorf("%q 应判定为本地文件引用", hint)
		}
	}
	network := []string{
		"console.bce.baidu.com",                     // 实测被正确拒的目标（不在授权范围）
		"adp.tencentcloud.com",                      // 授权范围主机
		"example.com",                               // 两段域名
		"https://example.com/x",                     // URL 形态
		"example.com/path.py",                       // URL 路径形态（主机在前）
		"example.com:8443/x",                        // 带端口
		"example.com:8443",                          // 带端口无路径
		"10.0.0.5",                                  // IP 字面量
		"hostmap",                                   // 无扩展名——形态不明，保守网络
		"my.data",                                   // 扩展名不在白名单
		"page.html, console.bce.baidu.com",          // 混入主机段 ⇒ 整体网络
		"file:///etc/passwd",                        // file:// 也保守按网络
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
	// 非出网：正则字面量、字符串数据、本地分析。
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
	// 出网：网络库/抓取工具的调用形态。
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

// TestScopePassFileTarget 钉住构造放行的三重前置（0.15.48）：快路径产物不放
// （hint 一定是主机）、有出网能力的载荷不放、主机形目标不放。
func TestScopePassFileTarget(t *testing.T) {
	localPayload := []byte(`python3 - <<'EOF'` + "\n" + `inet=re.compile(r'https?://x')` + "\n" + `EOF`)
	egressPayload := []byte(`python3 -c "urllib.request.urlopen('https://evil.com')"`)
	if !scopePassFileTarget("page__.html", localPayload, false) {
		t.Fatal("无出网能力载荷的文件形目标应构造放行")
	}
	if scopePassFileTarget("page__.html", egressPayload, false) {
		t.Fatal("有出网能力的载荷不得放行文件形目标")
	}
	if scopePassFileTarget("console.bce.baidu.com", localPayload, false) {
		t.Fatal("主机形目标不得构造放行（照常 scope 裁决）")
	}
	if scopePassFileTarget("example.com", localPayload, true) {
		t.Fatal("快路径产物的 hint 一定是主机，不得按文件形放行")
	}
}
