package pathguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// appLikeWorkspace 造出与真实部署同形的工作目录：
//
//	<base>/app/jwt.key          ← 平台签名密钥
//	<base>/app/data/sessions/<key>  ← agent 工作目录（三层 .. 才回到 app）
//
// 真实部署里工作目录是 /app/data/sessions/<key>，所以「../../../jwt.key」
// 正是经 shell 展开后才现形的那个绕过形态。
func appLikeWorkspace(t *testing.T) (work string, secret string) {
	t.Helper()
	base := t.TempDir()
	appRoot := filepath.Join(base, "app")
	work = filepath.Join(appRoot, "data", "sessions", "key1")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	secret = filepath.Join(appRoot, "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return work, secret
}

// 入参里的相对路径必须按工具工作目录解析后判定 —— 字符串预筛挡不住
// `cat ../../../jwt.key` 这类经 shell 展开才现形的路径。
func TestCheckInputPathsResolvesRelativeToWorkingDir(t *testing.T) {
	work, secret := appLikeWorkspace(t)
	withSecretRoots(t, secret)

	cases := []struct {
		desc string
		in   string
	}{
		{"相对回溯", `{"command":"cat ../../../jwt.key"}`},
		{"命令替换包裹", `{"command":"cat $(realpath ../../../jwt.key)"}`},
		{"变量前缀拼接", `{"command":"cat $HOME/../../../jwt.key"}`},
		{"绝对路径", `{"command":"cat ` + secret + `"}`},
		{"软链创建+读取", `{"command":"ln -s ` + secret + ` /tmp/x; cat /tmp/x"}`},
		{"文件工具参数", `{"file_path":"../../../jwt.key"}`},
	}
	for _, c := range cases {
		if err := CheckInputPaths("Bash", json.RawMessage(c.in), work); err == nil {
			t.Fatalf("%s：入参 %s 应被拒绝", c.desc, c.in)
		}
	}
}

// proc 族：环境块/命令行/内存都是平台秘密的入口。
func TestCheckInputPathsBlocksProcSecrets(t *testing.T) {
	work, secret := appLikeWorkspace(t)
	withSecretRoots(t, secret)

	for _, in := range []string{
		`{"command":"cat /proc/1/environ"}`,
		`{"command":"cat /proc/self/environ"}`,
		`{"command":"cat /proc/9/mem"}`,
		`{"file_path":"/proc/1234/cmdline"}`,
	} {
		if err := CheckInputPaths("Bash", json.RawMessage(in), work); err == nil {
			t.Fatalf("入参 %s 应被拒绝", in)
		}
	}
}

// 正常路径不得误伤：这是渗透 agent 的日常操作，一刀切会让平台不可用。
func TestCheckInputPathsAllowsNormalPaths(t *testing.T) {
	work, secret := appLikeWorkspace(t)
	withSecretRoots(t, secret)

	ok := []string{
		`{"command":"ls -la"}`,
		`{"command":"cat ./notes.md"}`,
		`{"command":"curl -s http://127.0.0.1:8787/api/health"}`,
		`{"command":"cat /etc/hosts"}`,
		`{"command":"find . -name '*.txt'"}`,
		`{"command":"nmap -sV 10.0.0.1"}`,
		`{"file_path":"/tmp/output.txt"}`,
		`{"command":"cat /app/data/sessions/key1/report.md"}`,
	}
	for _, in := range ok {
		if err := CheckInputPaths("Bash", json.RawMessage(in), work); err != nil {
			t.Fatalf("入参 %s 不应被拒绝：%v", in, err)
		}
	}
}

// URL 里的路径片段不是文件路径：/app/jwt.key 出现在 http URL 中不得触发误伤。
//
// 这里的秘密清单取真实的 /app/jwt.key —— 只有这样这条用例才有鉴别力：
// 若实现没有先剥掉 URL，这段路径会被当成文件路径而误伤。
func TestCheckInputPathsIgnoresURLPathSegments(t *testing.T) {
	work, _ := appLikeWorkspace(t)
	withSecretRoots(t, "/app/jwt.key")

	in := `{"command":"curl -s http://example.com/app/jwt.key"}`
	if err := CheckInputPaths("Bash", json.RawMessage(in), work); err != nil {
		t.Fatalf("URL 内的路径片段不应触发拒绝：%v", err)
	}
}

// 通过软链读平台密钥：候选串本身不是秘密路径（只有解析后才是），所以必须
// 真的去解析真实路径。
func TestCheckInputPathsResolvesSymlinkToSecret(t *testing.T) {
	work, secret := appLikeWorkspace(t)
	withSecretRoots(t, secret)

	link := filepath.Join(work, "link.key")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	in := `{"command":"cat ./link.key"}`
	if err := CheckInputPaths("Bash", json.RawMessage(in), work); err == nil {
		t.Fatal("指向平台密钥的软链应被拒绝")
	}
}
