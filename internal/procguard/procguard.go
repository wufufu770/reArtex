// Package procguard 给平台进程自身加一层 /proc 保护。
//
// 做什么：prctl(PR_SET_DUMPABLE, 0) 把平台进程变成「不可 dump」。效果是内核
// 把它的 /proc/<pid>/* 归到 root 所有，并拒绝没有 CAP_SYS_PTRACE 的进程
// ptrace attach 它 —— 于是 agent 的子进程读不到平台的环境块
// （/proc/<pid>/environ）、文件描述符（/proc/<pid>/fd）与内存
// （/proc/<pid>/mem），哪怕它与平台同 UID。
//
// 为什么用 prctl 而不是给 /proc 挂 hidepid=2：
//
//	hidepid=2 需要给 /proc 重新挂载（mount --bind / mount -t proc），而生产
//	容器跑在 docker 默认 AppArmor profile 下 —— 那条 profile 全局禁止 mount(2)，
//	实测在命名空间里 mount --bind 直接返回 EPERM，capability 也覆盖不了已加载
//	的 profile。prctl 不需要任何 capability、不需要重新挂载、不依赖容器策略，
//	因此能原样迁到生产。
//
//	hidepid=2 仍可作为「将来确实拿到挂载权限时的额外加固」叠加使用；它挡的是
//	「别的用户看见平台进程的 /proc 条目」，与本包挡的「平台进程不可被 dump」
//	是两条不同的保护。
package procguard
