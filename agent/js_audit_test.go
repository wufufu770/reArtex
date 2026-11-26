// js_audit_test.go：B-08A 验收——临时 workspace 纯本地测试，零网络。逐项覆盖
// 卡验收：lock 优先（v3/v1 两代）、无 lock 范围保留、devDependencies、
// workspaces、多包去重、无 package.json 空结果、非法 JSON、软链接逃逸拒绝、
// 超深目录/过大文件/清单数量限制、unsupported 形态不强转、node_modules 剪除、
// root 纪律。
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsWrite 相对 root 写文件（自动建目录）。
func jsWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsDepsOf(d *JSDiscovery, name string) []JSDep {
	var out []JSDep
	for _, dep := range d.Deps {
		if dep.Name == name {
			out = append(out, dep)
		}
	}
	return out
}

func jsMustOne(t *testing.T, d *JSDiscovery, name string) JSDep {
	t.Helper()
	got := jsDepsOf(d, name)
	if len(got) != 1 {
		t.Fatalf("%s 应恰 1 条，得到 %d: %+v", name, len(got), d.Deps)
	}
	return got[0]
}

// 锁文件优先：v3 packages 命中 → Locked=true 取锁版本；锁未命中/锁版本畸形 →
// 范围串保留 Locked=false；devDependencies 标记；v1 旧格式根级兜底。
func TestDiscoverJSDepsLockPriority(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{
		"name":"root",
		"dependencies":{"lodash":"^4.17.20","express":"^4.18.0","left-pad":"^1.3.0"},
		"devDependencies":{"typescript":"^5.0.0"}
	}`)
	jsWrite(t, root, "package-lock.json", `{
		"lockfileVersion":3,
		"packages":{
			"":{"name":"root"},
			"node_modules/lodash":{"version":"4.17.21"},
			"node_modules/typescript":{"version":"5.1.6"},
			"node_modules/left-pad":{"version":"not-a-semver"}
		}
	}`)
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	lodash := jsMustOne(t, d, "lodash")
	if !lodash.Locked || lodash.Version != "4.17.21" || lodash.Reason != "" {
		t.Fatalf("lodash 应锁 4.17.21 无备注: %+v", lodash)
	}
	express := jsMustOne(t, d, "express")
	if express.Locked || express.Version != "^4.18.0" || express.Reason != "" {
		t.Fatalf("express 应保留范围串 Locked=false: %+v", express)
	}
	ts := jsMustOne(t, d, "typescript")
	if !ts.Dev || !ts.Locked || ts.Version != "5.1.6" {
		t.Fatalf("typescript 应 dev+锁定: %+v", ts)
	}
	// 锁里出现畸形版本串：绝不原样采信 → 回落范围串 Locked=false。
	lp := jsMustOne(t, d, "left-pad")
	if lp.Locked || lp.Version != "^1.3.0" {
		t.Fatalf("畸形锁版本绝不采信: %+v", lp)
	}
	if lodash.Ecosystem != "npm" {
		t.Fatalf("ecosystem 恒 npm: %+v", lodash)
	}

	// v1 旧锁：根级 dependencies 兜底。
	root2 := t.TempDir()
	jsWrite(t, root2, "package.json", `{"dependencies":{"mkdirp":"^0.5.1"}}`)
	jsWrite(t, root2, "package-lock.json", `{"lockfileVersion":1,"dependencies":{"mkdirp":{"version":"0.5.6"}}}`)
	d2, err := DiscoverJSDeps(root2)
	if err != nil {
		t.Fatal(err)
	}
	m := jsMustOne(t, d2, "mkdirp")
	if !m.Locked || m.Version != "0.5.6" {
		t.Fatalf("v1 锁应命中: %+v", m)
	}
}

// workspaces 递归 + 跨清单多包去重（ecosystem+name+version 同键只记一条，
// 首见为准——BFS 根先行）；对象形 workspaces 同样支持。
func TestDiscoverJSDepsWorkspacesAndDedup(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{
		"name":"monorepo","workspaces":["packages/*"],
		"dependencies":{"lodash":"^4.17.20"}
	}`)
	jsWrite(t, root, "package-lock.json", `{
		"lockfileVersion":3,"packages":{"node_modules/lodash":{"version":"4.17.21"}}
	}`)
	jsWrite(t, root, "packages/app/package.json", `{
		"name":"app","dependencies":{"react":"^18.2.0","lodash":"4.17.21"}
	}`)
	jsWrite(t, root, "packages/app/package-lock.json", `{
		"lockfileVersion":3,"packages":{"packages/app/node_modules/react":{"version":"18.2.45"}}
	}`)
	jsWrite(t, root, "packages/web/package.json", `{"name":"web","dependencies":{"react":"^18.2.0"}}`)
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	// react 两条（app 锁定 18.2.45 / web 范围 ^18.2.0——版本不同不合并）。
	got := jsDepsOf(d, "react")
	if len(got) != 2 {
		t.Fatalf("react 应 2 条: %+v", got)
	}
	if got[0].Manifest != "packages/app/package.json" || !got[0].Locked || got[0].Version != "18.2.45" {
		t.Fatalf("app react 应由成员锁定版: %+v", got[0])
	}
	if got[1].Locked || got[1].Version != "^18.2.0" {
		t.Fatalf("web react 应保留范围串: %+v", got[1])
	}
	// 跨清单去重：根 lodash（锁 4.17.21）与 app lodash（精确 spec 4.17.21、
	// 无成员锁）同 eco+name+version → 只记一条（根清单首见）。
	lodash := jsDepsOf(d, "lodash")
	if len(lodash) != 1 {
		t.Fatalf("lodash 同版本跨清单应去重为 1 条: %+v", lodash)
	}
	if lodash[0].Manifest != "package.json" {
		t.Fatalf("去重应保留根清单首见: %+v", lodash[0])
	}
	// 对象形 workspaces。
	root2 := t.TempDir()
	jsWrite(t, root2, "package.json", `{"workspaces":{"packages":["apps/*"]},"dependencies":{"x":"^1.0.0"}}`)
	jsWrite(t, root2, "apps/a/package.json", `{"dependencies":{"y":"^2.0.0"}}`)
	d2, err := DiscoverJSDeps(root2)
	if err != nil {
		t.Fatal(err)
	}
	jsMustOne(t, d2, "y")
}

// unsupported 形态绝不从 spec 强转精确版本：有锁=取锁并如实标注；无锁=保留
// 原 spec + unsupported 原因。
func TestDiscoverJSDepsUnsupportedSpecForms(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{
		"dependencies":{
			"alias-locked":"npm:lodash@^4.17.20",
			"alias-bare":"npm:foo@^2.0.0",
			"gitdep":"git+https://github.com/o/r.git#v1.0.0",
			"filedep":"file:../local",
			"linkdep":"link:../local",
			"wsdep":"workspace:*",
			"ghdep":"github:o/r",
			"stardep":"*",
			"emptydep":""
		}
	}`)
	jsWrite(t, root, "package-lock.json", `{
		"lockfileVersion":3,
		"packages":{"node_modules/alias-locked":{"version":"4.17.21"}}
	}`)
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	al := jsMustOne(t, d, "alias-locked")
	if !al.Locked || al.Version != "4.17.21" || !strings.Contains(al.Reason, "别名") || !strings.Contains(al.Reason, "锁文件") {
		t.Fatalf("别名+锁：应取锁版本并如实标注: %+v", al)
	}
	for name, wantReason := range map[string]string{
		"alias-bare": "别名",
		"gitdep":     "git/URL",
		"filedep":    "file/link",
		"linkdep":    "file/link",
		"wsdep":      "workspace",
		"ghdep":      "git/URL",
		"stardep":    "通配",
		"emptydep":   "通配",
	} {
		got := jsMustOne(t, d, name)
		if got.Locked {
			t.Fatalf("%s 无锁绝不能 Locked=true: %+v", name, got)
		}
		if !strings.Contains(got.Reason, wantReason) || !strings.Contains(got.Reason, "unsupported") {
			t.Fatalf("%s 应标 unsupported %q: %+v", name, wantReason, got)
		}
	}
	// 未强转：git 依赖的版本串保留原 spec（#v1.0.0 不会被解析成精确版本）。
	if g := jsMustOne(t, d, "gitdep"); !strings.HasPrefix(g.Version, "git+") {
		t.Fatalf("git 依赖应保留原 spec: %+v", g)
	}
}

// 无 package.json → 空结果（Deps/Skipped 恒非 nil）；node_modules/.git 整枝剪除。
func TestDiscoverJSDepsEmptyAndPruned(t *testing.T) {
	empty := t.TempDir()
	d, err := DiscoverJSDeps(empty)
	if err != nil {
		t.Fatal(err)
	}
	if d.Deps == nil || d.Skipped == nil || len(d.Deps) != 0 || len(d.Skipped) != 0 {
		t.Fatalf("无清单应合法空列表非 nil: %+v", d)
	}

	// node_modules 里的第三方清单不是工作区自有清单面：剪除、不记 skip。
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{"dependencies":{"x":"^1.0.0"}}`)
	jsWrite(t, root, "node_modules/foo/package.json", `{"dependencies":{"injected":"^9.9.9"}}`)
	jsWrite(t, root, ".git/modules/weird/package.json", `{"dependencies":{"gitinner":"^1.0.0"}}`)
	d2, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsDepsOf(d2, "injected"); len(got) != 0 {
		t.Fatalf("node_modules 应剪除: %+v", got)
	}
	if got := jsDepsOf(d2, "gitinner"); len(got) != 0 {
		t.Fatalf(".git 应剪除: %+v", got)
	}
	jsMustOne(t, d2, "x")
	for _, s := range d2.Skipped {
		if strings.Contains(s.Path, "node_modules") {
			t.Fatalf("node_modules 剪除不是失败，不应记 skip: %+v", s)
		}
	}
}

// 非法 JSON：如实记 skip，绝不静默；其余合法清单照常解析、绝不半解析。
func TestDiscoverJSDepsInvalidJSON(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{"dependencies":{"ok":"^1.0.0"}}`)
	jsWrite(t, root, "broken/package.json", `{"name": [}]`)
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0].Reason, "非法 JSON") || d.Skipped[0].Path != "broken/package.json" {
		t.Fatalf("非法清单应记 skip: %+v", d.Skipped)
	}
	jsMustOne(t, d, "ok")
}

// 软链接逃逸拒绝：清单/锁文件软链接与目录软链接都不跟随，外部内容绝不进结果。
func TestDiscoverJSDepsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	secret := filepath.Join(parent, "secret")
	for _, dir := range []string{root, secret} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	jsWrite(t, root, "package.json", `{"dependencies":{"inside":"^1.0.0"}}`)
	jsWrite(t, secret, "package.json", `{"dependencies":{"leaked":"^9.9.9"}}`)
	jsWrite(t, secret, "package-lock.json", `{"lockfileVersion":3,"packages":{"node_modules/inside":{"version":"0.0.1-evil"}}}`)
	// ① 清单软链接：root/linkdir/package.json → secret/package.json
	if err := os.MkdirAll(filepath.Join(root, "linkdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(secret, "package.json"), filepath.Join(root, "linkdir", "package.json")); err != nil {
		t.Fatal(err)
	}
	// ② 目录软链接：root/dirlink → secret（ReadDir Lstat 语义不入队）
	if err := os.Symlink(secret, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	// ③ 锁文件软链接
	if err := os.Symlink(filepath.Join(secret, "package-lock.json"), filepath.Join(root, "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsDepsOf(d, "leaked"); len(got) != 0 {
		t.Fatalf("软链接逃逸内容绝不能进结果: %+v", got)
	}
	if got := jsMustOne(t, d, "inside"); got.Locked || got.Version != "^1.0.0" {
		t.Fatalf("锁文件软链接不跟随——inside 绝不能被外部锁版本污染: %+v", got)
	}
	escaped := 0
	for _, s := range d.Skipped {
		if strings.Contains(s.Reason, "软链接") {
			escaped++
		}
	}
	if escaped != 2 { // 清单软链接 + 锁文件软链接（目录软链接不入队不记 skip）
		t.Fatalf("应恰 2 条软链接 skip（清单+锁）: %+v", d.Skipped)
	}
}

// 超深目录剪除（首个被剪层若含清单则如实记 skip）+ 过大清单/锁文件拒绝。
func TestDiscoverJSDepsCaps(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{"dependencies":{"root-dep":"^1.0.0"}}`)
	// 超深：恰在第一个被剪层（深度 jsMaxDepth+1）放合法清单。
	deep := ""
	for i := 0; i < jsMaxDepth+1; i++ {
		deep = filepath.Join(deep, "n")
	}
	jsWrite(t, root, filepath.ToSlash(filepath.Join(deep, "package.json")), `{"dependencies":{"deep-dep":"^1.0.0"}}`)
	// 过大清单：>1MiB 的合法 JSON（形态合法但超预算）。
	huge := `{"dependencies":{"x":"^1.0.0","pad":"` + strings.Repeat("a", jsMaxFileBytes) + `"}}`
	jsWrite(t, root, "huge/package.json", huge)
	// 过大锁文件：>1MiB。
	hugeLock := `{"lockfileVersion":3,"packages":{"pad":"` + strings.Repeat("b", jsMaxFileBytes) + `"}}`
	jsWrite(t, root, "huge/package-lock.json", hugeLock)

	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsDepsOf(d, "deep-dep"); len(got) != 0 {
		t.Fatalf("超深清单绝不能被发现: %+v", got)
	}
	if got := jsDepsOf(d, "x"); len(got) != 0 {
		t.Fatalf("过大清单绝不能被解析: %+v", got)
	}
	jsMustOne(t, d, "root-dep")
	hasDepth, hasOversize := false, false
	for _, s := range d.Skipped {
		if strings.Contains(s.Reason, "超深度上限") {
			hasDepth = true
		}
		if strings.Contains(s.Reason, "过大") {
			hasOversize = true
		}
	}
	if !hasDepth || !hasOversize {
		t.Fatalf("超深与过大都应记 skip: %+v", d.Skipped)
	}
}

// 清单数量封顶：超过 jsMaxManifestFiles 的部分如实记 skip 且绝不解析。
func TestDiscoverJSDepsManifestCountCap(t *testing.T) {
	root := t.TempDir()
	jsWrite(t, root, "package.json", `{"dependencies":{"first":"^1.0.0"}}`)
	// 上限以内：逐清单唯一依赖名（互不去重）→ 全部解析。
	for i := 0; i < jsMaxManifestFiles-1; i++ {
		jsWrite(t, root, "p"+string(rune('a'+i%26))+string(rune('a'+i/26))+"/package.json",
			`{"dependencies":{"d`+itoa2(i)+`":"^1.0.0"}}`)
	}
	// 第 jsMaxManifestFiles 个清单：超限 → skip + 绝不解析（z 前缀保证 BFS
	// 顺序排在 p* 之后，恰好落在被闸拦下的位置）。
	jsWrite(t, root, "zoverflow/package.json", `{"dependencies":{"overflow-dep":"^1.0.0"}}`)
	d, err := DiscoverJSDeps(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Deps) != jsMaxManifestFiles { // first + d0..d498
		t.Fatalf("恰上限条清单应全部解析: %d", len(d.Deps))
	}
	if got := jsDepsOf(d, "overflow-dep"); len(got) != 0 {
		t.Fatalf("超限清单绝不解析: %+v", got)
	}
	found := false
	for _, s := range d.Skipped {
		if s.Path == "zoverflow/package.json" && strings.Contains(s.Reason, "清单数量超上限") {
			found = true
		}
	}
	if !found {
		t.Fatalf("超限应记 skip: %+v", d.Skipped)
	}
}

func itoa2(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for ; i > 0; i /= 10 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
	}
	return string(digits)
}

// root 纪律：相对路径/空串/不存在路径拒绝（生产调用方=既有 pathguard 绝对路径）。
func TestDiscoverJSDepsRootGuard(t *testing.T) {
	if _, err := DiscoverJSDeps("relative/path"); err == nil {
		t.Fatal("相对 root 必须拒绝")
	}
	if _, err := DiscoverJSDeps(""); err == nil {
		t.Fatal("空 root 必须拒绝")
	}
	if _, err := DiscoverJSDeps(filepath.Join(t.TempDir(), "not-exist")); err == nil {
		t.Fatal("不存在 root 必须报错")
	}
}
