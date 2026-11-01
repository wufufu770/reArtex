//go:build linux

package sod

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// 这个文件钉住 SoD 裁决与**进程特权**无关。
//
// 为什么这条要紧：平台在拉起子进程时会降权（internal/agentcred.ProcAttr 给
// SysProcAttr 设降权身份）。若裁决依赖进程的特权状态（uid/gid/是否 root），
// 那么「被降权的执行」与「没降权的执行」会得到不同结论 —— 攻击者只要能影响
// 自己跑在什么身份下，就能让同一条命令从「拒绝」变成「放行」。
//
// 两条腿：
//  1. 行为对照（这个文件）：同一套装配、同一份输入，在**另一个特权上下文**
//     里（新建 user namespace ⇒ 容器内 uid=0，与父进程不同）裁决一次，
//     两次结论必须一致。
//  2. 结构对照（e2e_test.go）：决策路径里根本没有读特权状态的代码。

const (
	// sodE2EChildTaskEnv / sodE2EChildHostEnv 把父进程的任务与目标传给子进程。
	sodE2EChildTaskEnv = "ARTEX_SOD_E2E_TASK_ID"
	sodE2EChildHostEnv = "ARTEX_SOD_E2E_HOST"
	// sodE2EVerdictPrefix / sodE2EUIDPrefix 是子进程回传结果的前缀。
	sodE2EVerdictPrefix = "SOD_E2E_VERDICT="
	sodE2EUIDPrefix     = "SOD_E2E_UID="
)

// TestSoDE2EPrivilegeChild 是特权对照的**子进程**用例：父进程不直接跑它。
//
// 它做的是与父进程完全相同的一次裁决（同一套装配、同一份载荷、同一个任务），
// 只是跑在另一个特权上下文里。
func TestSoDE2EPrivilegeChild(t *testing.T) {
	if os.Getenv(sodE2EChildEnv) != "1" {
		t.Skip("只有父进程会启动这个特权对照子进程")
	}
	d := auditorTestDB(t)
	taskID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(sodE2EChildTaskEnv)), 10, 64)
	if err != nil || taskID <= 0 {
		t.Fatalf("子进程没有拿到任务号: %v", err)
	}
	host := strings.TrimSpace(os.Getenv(sodE2EChildHostEnv))
	if host == "" {
		t.Fatal("子进程没有拿到目标 host")
	}

	chain, _, _ := sodE2EChain(t, d, taskID, host, sodE2EOptions{scopeInScope: true}, nil)
	ctx := WithRunContext(context.Background(), RunContext{TaskID: taskID, AgentKey: "worker"})
	block, msg, _ := chain.PreToolUse(ctx, "Bash", sodE2EPayload(host))

	verdict := "allow"
	if block {
		verdict = "block:" + msg
	}
	fmt.Printf("%s%s\n", sodE2EVerdictPrefix, verdict)
	fmt.Printf("%s%d\n", sodE2EUIDPrefix, os.Getuid())
}

// TestSoDE2EVerdictIndependentOfProcessPrivilege 钉住「裁决与进程特权无关」。
//
// 做法：父进程裁决一次，再让**同一个测试二进制**在另一个特权上下文里裁决一次
// （新建 user namespace，把宿主 uid 映射成容器内的 uid 0 ⇒ 容器内是 root，
// 与父进程的普通用户身份不同），两次结论必须一致。
//
// 为什么用 user namespace 而不是 setuid：降权（SysProcAttr.Credential）需要
// root 才能换 uid；user namespace 里的 uid 映射是**无特权**用户就能做的，而且
// 它同样改变了进程的特权上下文（容器内 uid=0、持有该命名空间内的全部能力）。
// 结论「换个特权上下文结论不变」正是要钉的性质。
func TestSoDE2EVerdictIndependentOfProcessPrivilege(t *testing.T) {
	env := newSoDE2E(t, sodE2EOptions{scopeInScope: true})

	block, msg, _ := env.chain.PreToolUse(env.ctx, "Bash", env.payload)
	parentVerdict := "allow"
	if block {
		parentVerdict = "block:" + msg
	}

	childVerdict, childUID, err := runSoDE2EPrivilegeChild(t, env)
	if err != nil {
		t.Skipf("起不了特权对照子进程（本机不允许 user namespace）: %v", err)
	}
	if childUID == os.Getuid() {
		t.Fatalf("子进程的 uid 与父进程相同 (%d) ⇒ 特权上下文没变, 这个对照没有意义", childUID)
	}
	if childVerdict != parentVerdict {
		t.Fatalf("特权上下文不同 ⇒ 裁决变了: 父进程=%q 子进程=%q", parentVerdict, childVerdict)
	}
}

// runSoDE2EPrivilegeChild 在另一个特权上下文里跑一次裁决，回传结论与 uid。
func runSoDE2EPrivilegeChild(t *testing.T, env *sodE2E) (string, int, error) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestSoDE2EPrivilegeChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		sodE2EChildEnv+"=1",
		sodE2EChildTaskEnv+"="+strconv.FormatInt(env.taskID, 10),
		sodE2EChildHostEnv+"="+env.host,
	)
	// 新建 user namespace：把宿主 uid/gid 映射成容器内的 0 ⇒ 子进程在容器里
	// 是 root（持有该命名空间内的全部能力），与父进程的普通用户身份不同。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", 0, fmt.Errorf("子进程退出码异常: %w；输出=%s", err, out)
	}
	verdict, uid, err := parseSoDE2EChildOutput(string(out))
	if err != nil {
		return "", 0, err
	}
	return verdict, uid, nil
}

// parseSoDE2EChildOutput 从子进程输出里取回结论与 uid。
func parseSoDE2EChildOutput(out string) (string, int, error) {
	var verdict string
	var uid = -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, sodE2EVerdictPrefix):
			verdict = strings.TrimPrefix(line, sodE2EVerdictPrefix)
		case strings.HasPrefix(line, sodE2EUIDPrefix):
			n, err := strconv.Atoi(strings.TrimPrefix(line, sodE2EUIDPrefix))
			if err != nil {
				return "", 0, fmt.Errorf("子进程回传的 uid 解析失败: %w", err)
			}
			uid = n
		}
	}
	if verdict == "" {
		return "", 0, fmt.Errorf("子进程没有回传裁决结论；输出=%s", out)
	}
	if uid < 0 {
		return "", 0, fmt.Errorf("子进程没有回传 uid；输出=%s", out)
	}
	return verdict, uid, nil
}
