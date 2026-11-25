package pathguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A-11（0.15.80）：统一真实路径解析 resolveReal(p, workingDir) 的验收用例。
//
// 覆盖卡片要求的四种符号链接夹具 + workingDir 相对路径口径 + tokens 裸文件名：
//   1. 工作区内软链指向平台秘密（既有 TestBlocksSymlinkEscape 已盖，这里盖目录形态）；
//   2. 软链指向工作区外的平台秘密（拒绝面 = 平台秘密清单；非秘密的外部路径
//      依设计放行——本包不做根目录收敛，见包注释，卡片此处按仓库现状落地）；
//   3. 新建文件落在软链化的父目录下；
//   4. 悬空符号链接：直接拒绝，不尝试跟随。

// wsDir 建一个带工作区语义的临时目录。
func wsDir(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// 裸文件名与目录软链：`cd <秘密目录> && cat jwt.key` 的等价形态——入参只有
// 相对名，真实落点由 workingDir 下的软链决定。秘密根登记为目录型（对齐生产里
// /app/data/traffic/_ca 的口径：目录内既有文件与新建文件都在拒绝面内）。
func TestCheckDeniesWorkingDirSymlinkEscape(t *testing.T) {
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secretDir)
	ws := wsDir(t)

	// 目录软链：ws/link → 秘密目录。
	link := filepath.Join(ws, "link")
	if err := os.Symlink(secretDir, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}

	// 既有文件经目录软链读秘密。
	if err := callWd(t, "Read", "file_path", "link/jwt.key", ws); err == nil {
		t.Fatal("经目录软链读秘密应被拒绝")
	}
	// 新建文件落在软链化的父目录下：真实落点在秘密目录内，同样拒绝。
	if err := callWd(t, "Write", "file_path", "link/poc.txt", ws); err == nil {
		t.Fatal("经目录软链写入秘密目录应被拒绝")
	}
}

// 软链指向工作区之外的平台秘密（/app 形态在容器里才有；这里用临时目录等价模拟
// 「工作区外的秘密」）。
func TestCheckDeniesSymlinkOutsideWorkspaceToSecret(t *testing.T) {
	outside := t.TempDir() // 工作区外的目录
	secret := filepath.Join(outside, ".env")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)
	ws := wsDir(t)

	link := filepath.Join(ws, "evil")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	if err := callWd(t, "Read", "file_path", "evil", ws); err == nil {
		t.Fatal("工作区内软链指向工作区外的秘密应被拒绝")
	}
	if err := callWd(t, "Read", "file_path", filepath.Base(link), ws); err == nil {
		t.Fatal("相对裸名经 workingDir 解析后指向秘密应被拒绝")
	}
}

// 悬空符号链接：目标不存在。读它必然失败，写它会在链接目标处落盘绕开判定——
// 一律拒绝，不尝试跟随（旧版会把它解析成「父目录+链接名」而放行）。
func TestCheckDeniesDanglingSymlink(t *testing.T) {
	withSecretRoots(t)
	ws := wsDir(t)
	link := filepath.Join(ws, "dangle")
	if err := os.Symlink(filepath.Join(ws, "gone-target"), link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	err := callWd(t, "Read", "file_path", "dangle", ws)
	if err == nil {
		t.Fatal("悬空符号链接应被拒绝")
	}
	if !strings.Contains(err.Error(), "悬空") {
		t.Fatalf("拒绝原因应点明悬空符号链接，实际: %v", err)
	}
	// Bash 入参路径片段的同形态：tokens 层同样拒绝。
	if err := CheckInputPaths("Bash", []byte(`{"command":"cat dangle"}`), ws); err == nil {
		t.Fatal("tokens 层的悬空符号链接也应被拒绝")
	}
}

// 相对路径按 workingDir（而不是进程 CWD）解析：工具真正打开文件用的就是它。
func TestCheckResolvesRelativeToWorkingDir(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)
	ws := wsDir(t)

	// ws/x → 秘密所在目录；入参 "x/jwt.key" 是相对路径。
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(ws, "x")); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	if err := callWd(t, "Read", "file_path", "x/jwt.key", ws); err == nil {
		t.Fatal("相对路径经 workingDir 解析后指向秘密应被拒绝")
	}

	// 合法工作流不受影响：工作区内的既有文件与新建文件照常放行。
	if err := os.WriteFile(filepath.Join(ws, "evidence", "notes.md"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := callWd(t, "Read", "file_path", "evidence/notes.md", ws); err != nil {
		t.Fatalf("工作区内合法相对读取被误伤: %v", err)
	}
	if err := callWd(t, "Write", "file_path", "evidence/new-find.md", ws); err != nil {
		t.Fatalf("工作区内新建文件被误伤: %v", err)
	}
	// workingDir 为空回落进程 CWD：绝对路径不受影响。
	if err := call(t, "Read", "file_path", filepath.Join(ws, "evidence", "notes.md")); err != nil {
		t.Fatalf("空 workingDir 下绝对路径读取被误伤: %v", err)
	}
}

// tokens 裸文件名补判：`cd <秘密目录> && cat jwt.key` / `cat .env` 这类
// 无路径起点的 token，此前整段被漏掉。
func TestCheckInputPathsBareFilename(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)

	if err := CheckInputPaths("Bash", []byte(`{"command":"cat jwt.key"}`), dir); err == nil {
		t.Fatal("裸文件名 jwt.key 按 workingDir 拼全后指向秘密应被拒绝")
	}

	envSecret := filepath.Join(dir, ".env")
	if err := os.WriteFile(envSecret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret, envSecret)
	if err := CheckInputPaths("Bash", []byte(`{"command":"cat .env"}`), dir); err == nil {
		t.Fatal("裸文件名 .env 按 workingDir 拼全后指向秘密应被拒绝")
	}

	// 真实路径层：裸名本身不命中清单，但它是软链、真实落点是秘密。
	ws := wsDir(t)
	if err := os.Symlink(secret, filepath.Join(ws, "target.key")); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	if err := CheckInputPaths("Bash", []byte(`{"command":"cat target.key"}`), ws); err == nil {
		t.Fatal("裸名软链指向秘密应经真实路径层拒绝")
	}

	// 不得误伤：非秘密目录下的同名裸词、以及大段文字里偶然出现的裸词。
	if err := CheckInputPaths("Bash", []byte(`{"command":"cat notes.md"}`), ws); err != nil {
		t.Fatalf("非秘密裸文件名被误伤: %v", err)
	}
	if err := CheckInputPaths("Write", []byte(`{"content":"报告：目标使用了弱 jwt.key 密钥"}`), ws); err != nil {
		t.Fatalf("正文里的裸词被误伤: %v", err)
	}
}
