package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/agentcred"
)

// 这组用例钉住「agent 的工具子进程与平台进程权限隔离」这条不变量：
// 子进程跑在专用非特权 UID 上、读不到平台的凭据文件、读不到平台的 /proc 条目、
// 拿不到平台的库连接串；同时它在自己的工作目录里仍然能写东西（干活没被砸掉）。

// requirePrivilegeDrop 跳过没有降权能力的运行环境。
// 把子进程 setuid 到别的 UID 需要 CAP_SETUID，只有 root 才有。
func requirePrivilegeDrop(t *testing.T) {
	t.Helper()
	if !agentcred.CanDrop() {
		t.Skip("需要 root（把子进程降权到 agent UID 需要 CAP_SETUID）")
	}
}

// runPrivilegeScript 以指定权限策略跑一段 python 脚本，回传子进程的输出。
func runPrivilegeScript(t *testing.T, workDir, code string, env []string, priv toolPrivilege) string {
	t.Helper()
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	out, err := execPython(context.Background(), pythonRun{
		Interp:    interp,
		Key:       "privtest",
		Code:      code,
		Params:    map[string]any{},
		WorkDir:   workDir,
		Env:       env,
		Timeout:   20 * time.Second,
		Privilege: priv,
	})
	if err != nil {
		t.Fatalf("execPython: %v", err)
	}
	return out
}

// childUID 从子进程输出里取回它看到的 uid。
func childUID(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "uid="); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("子进程输出的 uid 不是数字: %q", line)
			}
			return n
		}
	}
	t.Fatalf("子进程没有输出 uid:\n%s", out)
	return -1
}

// privWorkspace 建一个「父目录可穿行」的工作目录：降权后的子进程要能 chdir
// 进去，而 t.TempDir() 的父目录是 0700 的、子进程穿不过去（生产里的工作目录
// 挂在 <dataDir>/tasks/<taskID> 下，父目录都是 0755）。
//
// agentOwned=true 时目录归 agent 身份（子进程可写）；否则保持平台属主、
// 只放开穿行（子进程可进不可写）。
func privWorkspace(t *testing.T, agentOwned bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "artex-priv-")
	if err != nil {
		t.Fatalf("建工作目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if agentOwned {
		if err := agentcred.ChownToAgent(dir); err != nil {
			t.Fatalf("准备工作目录归属失败: %v", err)
		}
		return dir
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod 工作目录失败: %v", err)
	}
	return dir
}

// TestExecPythonChildRunsAsAgentUID：agent 调用路径上的 script 工具子进程
// 必须跑在专用非特权 UID 上，既不是 0（平台身份）也不是平台自己的 uid。
func TestExecPythonChildRunsAsAgentUID(t *testing.T) {
	requirePrivilegeDrop(t)
	dir := privWorkspace(t, true)
	out := runPrivilegeScript(t, dir, "import os\nprint('uid=%d' % os.getuid())\nprint('gid=%d' % os.getgid())\n", nil, privilegeAgent)
	uid := childUID(t, out)
	if uid != agentcred.AgentUID {
		t.Fatalf("子进程 uid = %d，期望 %d（agent 专用 UID）:\n%s", uid, agentcred.AgentUID, out)
	}
	if uid == 0 || uid == os.Geteuid() {
		t.Fatalf("子进程仍在平台身份上（uid=%d，平台 uid=%d）", uid, os.Geteuid())
	}
}

// TestExecPythonChildCannotReadPrivilegedSecret：平台自己的凭据文件
// （容器里是 /run/secrets/pg_dsn，0600、属主 uid 1000）子进程读不到。
func TestExecPythonChildCannotReadPrivilegedSecret(t *testing.T) {
	requirePrivilegeDrop(t)
	dir := privWorkspace(t, false)
	secret := filepath.Join(dir, "pg_dsn")
	if err := os.WriteFile(secret, []byte("postgres://artex:sup3rs3cret@db/artex"), 0o600); err != nil {
		t.Fatalf("写凭据文件失败: %v", err)
	}
	code := fmt.Sprintf(`import os
p = %q
try:
    open(p, "rb").read()
    print("secret=readable")
except OSError as e:
    print("secret=denied errno=%%d" %% e.errno)
`, secret)
	out := runPrivilegeScript(t, dir, code, nil, privilegeAgent)
	if !strings.Contains(out, "secret=denied") {
		t.Fatalf("子进程读到了平台凭据文件:\n%s", out)
	}
}

// TestExecPythonChildCannotReadPlatformProc：子进程读不到平台进程的
// /proc/<pid>/environ 与 /proc/<pid>/fd（前者装着平台的环境块，
// 后者装着平台打开的文件描述符）。
func TestExecPythonChildCannotReadPlatformProc(t *testing.T) {
	requirePrivilegeDrop(t)
	dir := privWorkspace(t, false)
	pid := os.Getpid()
	code := fmt.Sprintf(`import os
for p in ("/proc/%d/environ", "/proc/%d/fd"):
    try:
        open(p, "rb").read()
        print("proc=readable " + p)
    except OSError as e:
        print("proc=denied %%s errno=%%d" %% e.errno)
`, pid, pid)
	out := runPrivilegeScript(t, dir, code, nil, privilegeAgent)
	if strings.Contains(out, "proc=readable") {
		t.Fatalf("子进程读到了平台的 /proc 条目:\n%s", out)
	}
	if !strings.Contains(out, "proc=denied") {
		t.Fatalf("子进程没有报告 /proc 读取结果:\n%s", out)
	}
}

// TestExecPythonChildEnvHasNoPrivilegedDSN：平台的库连接串绝不能出现在
// 子进程环境里（白名单之外的凭据变量一律不下传）。
func TestExecPythonChildEnvHasNoPrivilegedDSN(t *testing.T) {
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:sup3rs3cret@db/artex")
	dir := privWorkspace(t, false)
	code := `import os
print("dsn=" + os.environ.get("ARTEX_PG_DSN", "<absent>"))
`
	out := runPrivilegeScript(t, dir, code, nil, privilegeAgent)
	if strings.Contains(out, "sup3rs3cret") {
		t.Fatalf("平台库连接串泄漏进子进程环境:\n%s", out)
	}
	if !strings.Contains(out, "dsn=<absent>") {
		t.Fatalf("子进程环境里应当没有 ARTEX_PG_DSN:\n%s", out)
	}
}

// TestExecPythonSessionEnvCannotSmugglePrivilegedDSN：会话 env（代理/CA 等，
// 由平台注入）同样不得把平台连接串带进子进程。
func TestExecPythonSessionEnvCannotSmugglePrivilegedDSN(t *testing.T) {
	dir := privWorkspace(t, false)
	code := `import os
print("dsn=" + os.environ.get("ARTEX_PG_DSN", "<absent>"))
`
	out := runPrivilegeScript(t, dir, code,
		[]string{"ARTEX_PG_DSN=postgres://artex:sup3rs3cret@db/artex"}, privilegeAgent)
	if strings.Contains(out, "sup3rs3cret") {
		t.Fatalf("会话 env 把平台库连接串带进了子进程:\n%s", out)
	}
}

// TestExecPythonChildCanWriteWorkspace：降权不能把 agent 的正常工作砸掉 ——
// 子进程在自己的工作目录里必须还能写文件。
func TestExecPythonChildCanWriteWorkspace(t *testing.T) {
	requirePrivilegeDrop(t)
	dir := privWorkspace(t, true)
	code := `import os
open("artifact.txt", "w").write("ok")
print("wrote=" + str(os.path.exists("artifact.txt")))
`
	out := runPrivilegeScript(t, dir, code, nil, privilegeAgent)
	if !strings.Contains(out, "wrote=True") {
		t.Fatalf("子进程在工作目录里写不了文件:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "artifact.txt")); err != nil {
		t.Fatalf("子进程没有产出文件: %v", err)
	}
}

// TestExecPythonHostRunKeepsPlatformPrivilege：host 侧的「试运行」是平台自身的
// 操作（不是 agent 调用），必须保留平台身份 —— 否则 host 上合法的脚本工具
// 预览会因为写不进项目目录而失效。
func TestExecPythonHostRunKeepsPlatformPrivilege(t *testing.T) {
	dir := privWorkspace(t, false)
	out := runPrivilegeScript(t, dir, "import os\nprint('uid=%d' % os.getuid())\n", nil, privilegePlatform)
	uid := childUID(t, out)
	if uid != os.Geteuid() {
		t.Fatalf("host 试运行的子进程 uid = %d，期望平台 uid %d", uid, os.Geteuid())
	}
}
