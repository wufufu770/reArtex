// Package agentcred 定义 agent 工具子进程的身份，以及把平台自己创建的文件
// 交给该身份所需的辅助函数。
//
// 为什么需要它：容器里平台进程以 root 运行（基础镜像默认 root），agent 的
// 工具子进程继承同样的身份 ⇒ 平台自己的凭据文件（docker secret 挂到
// /run/secrets/pg_dsn，模式 0600 但对 root 全开）对 agent 是可读的，agent 拿到
// 连接串就能绕过全部应用层 scope 校验直连数据库。把工具子进程降到专用非特权
// UID，这条读路径就断了。
//
// 为什么放 internal 叶子包：server（工具执行器）与 agent（工作目录归属）都要用，
// 而 db 依赖 agent —— 放叶子包才不会被依赖方向卡住（与 internal/scopepolicy 同理）。
package agentcred

import "os"

const (
	// AgentUID 是 agent 工具子进程专用的 UID。
	//
	// 为什么不是 0：0 是平台进程自己的身份，降到 0 等于没降。
	// 为什么不是 1000：容器里挂载出来的库凭据文件 /run/secrets/pg_dsn 属主就是
	// uid 1000、模式 0600 —— 用 1000 跑工具子进程，它照样读得走那份凭据，
	// 降权形同虚设。
	// 65534（nobody）是常见基础镜像里都存在的最后一个非特权 UID。
	AgentUID = 65534
	// AgentGID 是 agent 工具子进程专用的 GID，与 AgentUID 同值。
	AgentGID = 65534
)

// CanDrop 报告当前进程有没有能力把子进程降权到 agent 身份。
//
// setuid/setgid 需要 CAP_SETUID/CAP_SETGID，只有 root（或带这两个 capability
// 的进程）做得到。非 root 部署（桌面版、本地开发）返回 false —— 那时保持平台
// 身份不变，绝不因为「降不了权」把工具整个跑不起来。
func CanDrop() bool { return os.Geteuid() == 0 }

// ChownToAgent 把 path 的属主改成 agent 身份，让降权后的子进程能读写它。
//
// 只在能降权时才动手（CanDrop）：非 root 进程 chown 到别的 UID 会 EPERM，
// 而那时本来也不会降权，改了也没有意义。
func ChownToAgent(path string) error {
	if !CanDrop() {
		return nil
	}
	return os.Chown(path, AgentUID, AgentGID)
}
