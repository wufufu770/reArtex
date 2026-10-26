package pathguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSecretRoots 把秘密清单临时指向 t.TempDir()，让用例不依赖容器里真实存在的
// /app/jwt.key。secretRoots 本身是包级 var，测试直接替换并在 Cleanup 还原，
// 生产代码不需要为此留任何钩子。
func withSecretRoots(t *testing.T, roots ...string) {
	t.Helper()
	orig := secretRoots
	secretRoots = roots
	t.Cleanup(func() { secretRoots = orig })
}

func call(t *testing.T, tool, pathParam, path string) error {
	t.Helper()
	raw, err := json.Marshal(map[string]string{pathParam: path})
	if err != nil {
		t.Fatal(err)
	}
	return Check(tool, raw)
}

func TestBlocksDirectSecretPaths(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)
	for _, tool := range []string{"Read", "Write", "Edit", "MultiEdit"} {
		if err := call(t, tool, "file_path", secret); err == nil {
			t.Fatalf("%s 直读 %s 应被拒绝", tool, secret)
		}
	}
}

// 验收核心：短路表用字符串匹配，挡不住经软链现形的路径；这里用真实路径解析挡住。
// 这条用例通过即证明 A1 相对 b85d2d9 是实质改进而非重复。
func TestBlocksSymlinkEscape(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "jwt.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)

	link := filepath.Join(t.TempDir(), "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	err := call(t, "Read", "file_path", link)
	if err == nil {
		t.Fatal("软链指向 jwt.key 却放行——真实路径解析没生效")
	}
	if !strings.Contains(err.Error(), "凭据") {
		t.Fatalf("拒绝原因应指明是平台凭据，实际: %v", err)
	}
}

func TestBlocksDirectoryTraversal(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "app", "jwt.key")
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSecretRoots(t, secret)

	work := t.TempDir()
	t.Chdir(work)
	rel, err := filepath.Rel(work, secret)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("用例前提不成立: 期望得到含 .. 的相对路径, 实得 %q", rel)
	}
	if err := call(t, "Read", "file_path", rel); err == nil {
		t.Fatalf("相对路径 %q 经 .. 指向秘密 %s 却放行", rel, secret)
	}
}

func TestBlocksProcEnvironment(t *testing.T) {
	withSecretRoots(t)
	for _, p := range []string{
		"/proc/self/environ",
		"/proc/1/environ",
		"/proc/1234/cmdline",
		"/proc/1/mem",
	} {
		if err := call(t, "Read", "file_path", p); err == nil {
			t.Fatalf("%s 应被拒绝", p)
		}
	}
}

// 渗透工作流不能被误伤：agent 的工作区、证据目录、/tmp、系统侦察文件都必须放行。
// 这条用例对应 Oracle 的「不要做根目录收敛」——收在这里就是事故。
//
// 家目录用通用占位，不写真实用户路径：这些文件会进版本库。
func TestAllowsPentestPaths(t *testing.T) {
	withSecretRoots(t, "/app/jwt.key", "/app/.env", "/app/data/traffic/_ca")
	for _, p := range []string{
		"/app/data/sessions/abc123/notes.md",
		"/app/data/tasks/10/i267/cmd-output/output-179.txt",
		"/app/data/tasks/10/i267/sub_crtsh2.txt",
		"/tmp/nmap-out.xml",
		"/etc/passwd",
		"/var/www/html/index.php",
		"/home/analyst/workspace/intel/TARGET.md",
	} {
		for _, tool := range []string{"Read", "Write", "Edit"} {
			if err := call(t, tool, "file_path", p); err != nil {
				t.Fatalf("%s %s 被误伤: %v", tool, p, err)
			}
		}
	}
}

// 写入尚不存在的新文件是常态（PoC 脚本、扫描输出），必须靠「解析最深已存在祖先」
// 放行，而不是因为解析失败就拒绝。
func TestAllowsWriteToNewFileUnderWorkspace(t *testing.T) {
	withSecretRoots(t, "/app/jwt.key", "/app/.env", "/app/data/traffic/_ca")
	ws := t.TempDir()
	target := filepath.Join(ws, "poc", "exploit.py")
	if err := call(t, "Write", "file_path", target); err != nil {
		t.Fatalf("写入工作区内新文件被误伤: %v", err)
	}
}

func TestIgnoresNonPathTools(t *testing.T) {
	withSecretRoots(t, "/app/jwt.key")
	raw, err := json.Marshal(map[string]string{"command": "cat /app/jwt.key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Check("Bash", raw); err != nil {
		t.Fatalf("Bash 不由本包判定（命令串路径要经 shell 展开，交给短路表）: %v", err)
	}
}

func TestMalformedInputNotFatal(t *testing.T) {
	withSecretRoots(t)
	if err := Check("Read", []byte("{not json")); err != nil {
		t.Fatalf("畸形输入应放行并交工具自己报错: %v", err)
	}
}
