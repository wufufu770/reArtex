//go:build linux

package procguard

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// helperEnv 标记「本次进程是被拉起来做断言的辅助进程」。
const helperEnv = "ARTEX_PROCGUARD_HELPER"

// TestApplyMakesProcessNonDumpable 断言 prctl(PR_SET_DUMPABLE, 0) 真的落到
// 当前进程上：辅助进程里读回 PR_GET_DUMPABLE 必须从 true 变成 false，
// 且 /proc/self/environ 的属主变成 root。
//
// 之所以在辅助进程里做：PR_SET_DUMPABLE 改的是「调用者自己」，
// 在测试进程里直接改会污染整个测试二进制。
func TestApplyMakesProcessNonDumpable(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		return
	}
	out := runHelperProcess(t)
	if !strings.Contains(out, "before=true") {
		t.Fatalf("辅助进程执行前应当是 dumpable（before=true）:\n%s", out)
	}
	if !strings.Contains(out, "after=false") {
		t.Fatalf("Apply() 之后 PR_GET_DUMPABLE 必须是 false:\n%s", out)
	}
	if !strings.Contains(out, "environ_owner_uid=0") {
		t.Fatalf("Apply() 之后 /proc/self/environ 必须归 root 所有:\n%s", out)
	}
}

// TestApplyBlocksSameUIDProcAccess 断言降 dumpable 之后，同 UID 的另一个进程
// 连平台进程的 /proc/<pid>/mem 都打不开（读内存里的凭据靠的就是这条）。
//
// 这条在「没有 CAP_SYS_PTRACE」的环境里才有判别力：容器里 compose 已经
// cap_drop: [SYS_PTRACE]，本来就打不开；开发机上（uid 1000、ptrace_scope=0）
// 没有 prctl 时是打得开的，加了 prctl 才打不开 —— 那才是本用例的判别点。
func TestApplyBlocksSameUIDProcAccess(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		return
	}
	out := runHelperProcess(t)
	if !strings.Contains(out, "mem_open=denied") {
		t.Fatalf("Apply() 之后同 UID 进程应当打不开 /proc/<pid>/mem:\n%s", out)
	}
}

// runHelperProcess 拉起辅助进程并回传它的输出。
func runHelperProcess(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("辅助进程失败: %v\n%s", err, out)
	}
	return string(out)
}

// runHelper 是辅助进程的主体：降 dumpable，然后报告可观测后果。
func runHelper() {
	before, err := Dumpable()
	if err != nil {
		fmt.Println("helper_error=" + err.Error())
		os.Exit(3)
	}
	if err := Apply(); err != nil {
		fmt.Println("helper_error=" + err.Error())
		os.Exit(4)
	}
	after, err := Dumpable()
	if err != nil {
		fmt.Println("helper_error=" + err.Error())
		os.Exit(5)
	}
	fmt.Printf("before=%v after=%v\n", before, after)

	// /proc/self/environ 的属主：dumpable=0 之后归 root。
	if fi, err := os.Stat("/proc/self/environ"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			fmt.Printf("environ_owner_uid=%d\n", st.Uid)
		}
	}

	// 另起一个同 UID 进程去打开自己的 /proc/<pid>/mem：降 dumpable 之后应当失败。
	pid := os.Getpid()
	sh, err := exec.LookPath("sh")
	if err != nil {
		fmt.Println("mem_open=skipped")
		return
	}
	cmd := exec.Command(sh, "-c", "exec 3< /proc/"+strconv.Itoa(pid)+"/mem")
	if err := cmd.Run(); err != nil {
		fmt.Println("mem_open=denied")
		return
	}
	fmt.Println("mem_open=allowed")
}
