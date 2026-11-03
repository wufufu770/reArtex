package agent

// 后台命令跨 run 持久（方案书 session-groupchat-proposal.md §11.7 P0-④b）的宿主侧接线。
//
// 问题：vendored SDK 把后台命令的生命周期绑在**session**（= 一次 run）上。run 结束
// ⇒ SDK 的 Manager.Cleanup() 把它杀掉 ⇒ agent 承诺的「我稍后去读那个输出」落空。
// SDK 自己在 third_party/norma/tool/task.go 的 Cleanup 注释里就写明了：让输出真正
// 持久需要「落在宿主指定目录 + 由宿主决定保留多久」，而「that is a design change,
// not a change to this function」。
//
// §11.7 的六件套在本文件里的落点：
//
//	① 宿主目录        → init() 把部署口径的 ARTEX_TASK_DIR 映射到 SDK 认识的
//	                   NORMA_TASK_DIR（SDK 侧见 patches/norma-background-persist.patch）。
//	② 跨 session 注册表 → BgTaskRegistry + db.bg_task 表：按 ARTEX 任务 id 索引，
//	                   后来的 run 能查到先前 run 起的后台命令。
//	③ 真相来源=任务表  → 每个后台命令在 bg_task 里占一行（running/done/failed/lost）。
//	④ 回收者          → BgTaskRegistry.Reap + 进程级 janitor。
//	⑤ 完成提醒        → dbBgTaskReminder：投进任务的探索图（hint 节点，规划者下一轮
//	                   的「本轮态势」里读得到）+ 任务邮箱（群聊消息）。
//	⑥ lost 标记       → db.BgTaskStateLost，与 failed 分开。
//
// 顺序（§11.7 明说）：①+② 是前置；③ 与 ④ 必须同时上线 —— 只有记账没有回收就是进程
// 泄漏。这条约束在代码里落成：SDK 的 TaskObserver 接口把「记账」放在「拿到 owner
// key（= 允许持久）」之前 —— 账本写不进去 ⇒ 任务不持久 ⇒ 不会留下没人管的进程。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 后台命令的宿主侧旋钮。
const (
	// BgTaskDirEnv 是宿主指定的后台命令输出根目录。
	//
	// SDK 侧读的是 NORMA_TASK_DIR（vendored SDK 的命名空间），这里把部署口径的
	// ARTEX_TASK_DIR 映射过去：部署只需要认识 ARTEX_* 一个前缀。
	BgTaskDirEnv = "ARTEX_TASK_DIR"
	// BgTaskTTLEnv 是后台命令的存活上限（TTL）。到期由回收者收走。
	BgTaskTTLEnv = "ARTEX_BG_TASK_TTL"
	// BgTaskReapIntervalEnv 是回收者的轮询间隔。
	BgTaskReapIntervalEnv = "ARTEX_BG_TASK_REAP_INTERVAL"
)

const (
	// defaultBgTaskTTL 是 TTL 兜底的默认值（§11.7：默认 1 小时）。
	defaultBgTaskTTL = time.Hour
	// defaultBgTaskReapInterval 是回收者默认的轮询间隔。
	defaultBgTaskReapInterval = time.Minute
	// defaultBgTaskDirName 是宿主目录在 $HOME 下的默认位置（~/.artex/tasks）。
	defaultBgTaskDirName = ".artex/tasks"
)

func init() {
	// 宿主目录：ARTEX_TASK_DIR（部署配置）→ NORMA_TASK_DIR（SDK 认识的名字）。
	// 已经在环境里的 NORMA_TASK_DIR 优先（谁更具体谁赢），都没有就用默认的
	// ~/.artex/tasks。
	if strings.TrimSpace(os.Getenv("NORMA_TASK_DIR")) == "" {
		dir := strings.TrimSpace(os.Getenv(BgTaskDirEnv))
		if dir == "" {
			dir = defaultBgTaskDir()
		}
		_ = os.Setenv("NORMA_TASK_DIR", dir)
	}
	// 把宿主钩子装给 SDK：命令转后台的那一刻，SDK 会问我们要 owner key。
	actool.SetTaskObserver(bgTaskObserver{})
}

// defaultBgTaskDir 是宿主目录的默认位置：~/.artex/tasks。
func defaultBgTaskDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "artex", "tasks")
	}
	return filepath.Join(home, filepath.FromSlash(defaultBgTaskDirName))
}

// bgTaskTTL 读 TTL 旋钮。
//
// 接受 "1h" / "30m" 这种时长，也接受裸数字（按秒算）。非法或非正值一律退回默认的
// 1 小时 —— TTL 旋钮写错的后果是「后台命令活得比该活的久」，宁可保守。
func bgTaskTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv(BgTaskTTLEnv))
	if v == "" {
		return defaultBgTaskTTL
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if secs, err := parsePositiveInt(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	log.Printf("[bg_task] %s=%q 解析失败，回退默认 %s", BgTaskTTLEnv, v, defaultBgTaskTTL)
	return defaultBgTaskTTL
}

// bgTaskReapInterval 读回收者的轮询间隔旋钮。
func bgTaskReapInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv(BgTaskReapIntervalEnv))
	if v == "" {
		return defaultBgTaskReapInterval
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if secs, err := parsePositiveInt(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	return defaultBgTaskReapInterval
}

// parsePositiveInt 解析正整数（不接受负号与零）。
func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("不是正整数")
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, errors.New("太大")
		}
	}
	if n <= 0 {
		return 0, errors.New("不是正整数")
	}
	return n, nil
}

// ---------- 账本接口 ----------

// BgTaskStore 是后台命令账本的最小接口。
//
// 生产实现是 *db.DB（db/bg_task.go），测试用假实现 —— 回收者的判定逻辑因此可以
// 在不碰数据库、也不碰活进程的前提下被单测。
type BgTaskStore interface {
	EnsureBgTaskSchema() error
	InsertBgTask(ctx context.Context, t db.BgTask) (int64, error)
	UpdateBgTaskState(ctx context.Context, id int64, state string, exitCode *int, notice string, endedAt time.Time, notified bool) error
	GetBgTaskBySessionTask(ctx context.Context, sessionID, sdkTaskID string) (*db.BgTask, error)
	ListBgTasksByOwner(ctx context.Context, ownerTaskID int64) ([]db.BgTask, error)
	ListRunningBgTasks(ctx context.Context) ([]db.BgTask, error)
}

// BgTaskKiller 杀掉一个后台进程。生产实现按**进程组**杀（见 bg_task_unix.go）。
type BgTaskKiller func(pid int) error

// BgTaskReminder 是完成提醒的投递口。
type BgTaskReminder interface {
	// Deliver 把一条完成提醒投给拥有该任务的 agent。
	Deliver(ctx context.Context, row db.BgTask, notice string) error
}

// ---------- 注册表 ----------

// BgTaskRegistry 是后台命令的**跨 session 注册表**（宿主侧）。
//
// 它同时是三件事：
//   - SDK 的宿主钩子（OnBackgrounded / OnFinished）；
//   - 跨 session 的查询面（List：某个 ARTEX 任务名下有哪些后台命令）；
//   - 回收者（Reap：把过期还在跑的命令杀掉并标 lost）。
//
// store/ttl/now/kill 都可注入 ⇒ 判定逻辑可以脱离数据库与活进程单测。
type BgTaskRegistry struct {
	store   BgTaskStore
	remind  BgTaskReminder
	ttl     time.Duration
	now     func() time.Time
	kill    BgTaskKiller
	janitor sync.Once
}

// BgTaskRegistryOption 是注册表的可选项。
type BgTaskRegistryOption func(*BgTaskRegistry)

// WithBgTaskTTL 设 TTL（默认 1 小时）。
func WithBgTaskTTL(d time.Duration) BgTaskRegistryOption {
	return func(r *BgTaskRegistry) {
		if d > 0 {
			r.ttl = d
		}
	}
}

// WithBgTaskClock 换时钟（测试用）。
func WithBgTaskClock(now func() time.Time) BgTaskRegistryOption {
	return func(r *BgTaskRegistry) {
		if now != nil {
			r.now = now
		}
	}
}

// WithBgTaskKiller 换杀进程的实现（测试用）。
func WithBgTaskKiller(kill BgTaskKiller) BgTaskRegistryOption {
	return func(r *BgTaskRegistry) {
		if kill != nil {
			r.kill = kill
		}
	}
}

// WithBgTaskReminder 换完成提醒的投递口（测试用）。
func WithBgTaskReminder(remind BgTaskReminder) BgTaskRegistryOption {
	return func(r *BgTaskRegistry) {
		r.remind = remind
	}
}

// NewBgTaskRegistry 建一个注册表。store 为 nil 时用宿主自己的连接（按需打开）。
func NewBgTaskRegistry(store BgTaskStore, opts ...BgTaskRegistryOption) *BgTaskRegistry {
	r := &BgTaskRegistry{
		store: store,
		ttl:   bgTaskTTL(),
		now:   time.Now,
		kill:  killBgProcess,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// defaultBgRegistry 是宿主默认用的注册表（单例，按需建）。
var (
	defaultBgRegistryOnce sync.Once
	defaultBgRegistryVal  *BgTaskRegistry
	defaultBgRegistryErr  error
)

// bgRegistry 拿到宿主默认的注册表。
//
// 为什么自己开一条连接而不是复用 server 那条：server/assembly.go 装配的 *db.DB 在
// server 包里，本单元（agent/bg_task*.go）拿不到它，而 server/** 不是本单元的
// 地盘。于是这里按需开一条**小池**连接，只在第一条后台命令出现时才开（还没有后台
// 命令时，一条连接都不占）。
func bgRegistry() (*BgTaskRegistry, error) {
	defaultBgRegistryOnce.Do(func() {
		dsn, _, err := db.DSN()
		if err != nil {
			defaultBgRegistryErr = err
			return
		}
		d, err := db.Open(dsn)
		if err != nil {
			defaultBgRegistryErr = err
			return
		}
		// 这条连接只服务于后台命令的记账与回收，不参与业务查询 ⇒ 小池即可。
		d.SetMaxOpenConns(4)
		d.SetMaxIdleConns(2)
		defaultBgRegistryVal = NewBgTaskRegistry(d, WithBgTaskReminder(&dbBgTaskReminder{db: d}))
	})
	return defaultBgRegistryVal, defaultBgRegistryErr
}

// ---------- SDK 宿主钩子 ----------

// bgTaskObserver 是装给 SDK 的钩子。
type bgTaskObserver struct{}

// OnBackgrounded 在命令转后台的那一刻被 SDK 调用。
//
// 返回值就是 owner key：非空 = 这条命令**持久**（活过本 session），空 = 不持久
// （保持 SDK 的旧行为，进程随 session 一起死）。
//
// 顺序是刻意的：**先记账，再决定持久**。账本写不进去 ⇒ 回空串 ⇒ 命令不持久 ⇒
// 不会留下一个没人负责回收的进程。这正是 §11.7 说的「记账与回收必须同时上线」。
func (bgTaskObserver) OnBackgrounded(ctx context.Context, info actool.TaskInfo) string {
	r, err := bgRegistry()
	if err != nil || r == nil {
		// 拿不到账本 ⇒ 不持久。这是安全的降级：命令随 session 死，但不会泄漏。
		return ""
	}
	return r.OnBackgrounded(ctx, info)
}

// OnFinished 在后台命令结束时被 SDK 调用（完成 / 失败 / 被杀都算）。
func (bgTaskObserver) OnFinished(ctx context.Context, info actool.TaskInfo) {
	r, err := bgRegistry()
	if err != nil || r == nil {
		return
	}
	r.OnFinished(ctx, info)
}

// OnBackgrounded 登记一条后台命令，返回 owner key（空 = 不持久）。
func (r *BgTaskRegistry) OnBackgrounded(ctx context.Context, info actool.TaskInfo) string {
	store := r.store
	if store == nil {
		return ""
	}
	ri := RunInfoFrom(ctx)
	if ri.TaskID <= 0 {
		// 没有拥有者就没有 owner key ⇒ 不持久（纯聊天会话不属于任何任务）。
		return ""
	}
	if err := store.EnsureBgTaskSchema(); err != nil {
		log.Printf("[bg_task] 建表失败，后台命令不持久: %v", err)
		return ""
	}
	ownerKey := fmt.Sprintf("task:%d", ri.TaskID)
	ttl := r.ttl
	row := db.BgTask{
		OwnerTaskID:  ri.TaskID,
		OwnerKey:     ownerKey,
		SessionID:    info.SessionID,
		SdkTaskID:    info.ID,
		Command:      info.Command,
		OutputPath:   info.OutputPath,
		PID:          info.PID,
		State:        db.BgTaskStateRunning,
		DeadlineUnix: taskClockFrom(ctx).DeadlineUnix,
		TTLSeconds:   int(ttl / time.Second),
		StartedAt:    r.now(),
	}
	if _, err := store.InsertBgTask(ctx, row); err != nil {
		// 记账失败 ⇒ 不持久。没有账本就没有回收者 ⇒ 持久 = 泄漏。
		log.Printf("[bg_task] 登记后台命令失败（%s），该命令不持久: %v", info.ID, err)
		return ""
	}
	// 回收者从这一刻起必须有人跑：这一行是 running，而进程会活过本 session。
	r.StartJanitor(context.WithoutCancel(ctx))
	return ownerKey
}

// OnFinished 收尾一条后台命令的账本，并投递完成提醒。
func (r *BgTaskRegistry) OnFinished(ctx context.Context, info actool.TaskInfo) {
	store := r.store
	if store == nil {
		return
	}
	// run 的 ctx 可能已经取消（这一轮早就结束了）⇒ 收尾的写操作用脱离取消的 ctx。
	ctx = context.WithoutCancel(ctx)
	row, err := store.GetBgTaskBySessionTask(ctx, info.SessionID, info.ID)
	if err != nil {
		log.Printf("[bg_task] 查后台命令 %s 失败: %v", info.ID, err)
		return
	}
	if row == nil {
		return
	}
	state := db.BgTaskStateDone
	switch info.Status {
	case actool.TaskFailed:
		state = db.BgTaskStateFailed
	case actool.TaskKilled:
		// 被显式停掉（TaskStop）：不是「被回收」，所以是 failed 而不是 lost。
		state = db.BgTaskStateFailed
	}
	notice := bgCompletionNotice(info)

	// 完成提醒先投，投不出去也不能影响收尾（账本必须闭合）。
	notified := false
	if r.remind != nil {
		if err := r.remind.Deliver(ctx, *row, notice); err != nil {
			log.Printf("[bg_task] 完成提醒投递失败（%s）: %v", info.ID, err)
		} else {
			notified = true
		}
	}
	code := info.ExitCode
	if err := store.UpdateBgTaskState(ctx, row.ID, state, &code, notice, r.now(), notified); err != nil {
		log.Printf("[bg_task] 收尾后台命令 %s 失败: %v", info.ID, err)
	}
}

// bgCompletionNotice 是完成提醒的正文。
//
// 只带**路径**，不带输出内容：输出可能很大，模型要读就自己去读那个文件（SDK 的
// <task-notification> 也是这么做的）。
func bgCompletionNotice(info actool.TaskInfo) string {
	cmd := bgShortCommand(info.Command)
	switch info.Status {
	case actool.TaskCompleted:
		return fmt.Sprintf("你起的后台命令已完成（exit %d）：%s\n输出文件：%s\n"+
			"（输出可能还没写完，读之前先确认进程已退出；本提醒不携带输出内容。）",
			info.ExitCode, cmd, info.OutputPath)
	case actool.TaskFailed:
		return fmt.Sprintf("你起的后台命令失败（exit %d）：%s\n输出文件：%s",
			info.ExitCode, cmd, info.OutputPath)
	default:
		return fmt.Sprintf("你起的后台命令已结束（%s）：%s\n输出文件：%s",
			info.Status, cmd, info.OutputPath)
	}
}

// bgShortCommand 把命令压成一行短标签（提醒正文用）。
func bgShortCommand(cmd string) string {
	s := strings.TrimSpace(strings.ReplaceAll(cmd, "\n", " "))
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// ---------- 跨 session 查询面 ----------

// List 列出某个 ARTEX 任务名下的全部后台命令（跨 session）。
//
// 这就是「后来的 run 找到先前 run 起的后台命令」的那条路：按任务 id 查，输出路径
// 就在每一行里。
func (r *BgTaskRegistry) List(ctx context.Context, ownerTaskID int64) ([]db.BgTask, error) {
	if r.store == nil {
		return nil, errors.New("后台命令注册表未接线")
	}
	if err := r.store.EnsureBgTaskSchema(); err != nil {
		return nil, err
	}
	return r.store.ListBgTasksByOwner(ctx, ownerTaskID)
}

// ---------- 回收者 ----------

// BgReapResult 是一次回收的结果。
type BgReapResult struct {
	// Examined 是扫到的 running 行数。
	Examined int
	// Reaped 是被标记成 lost 的行数。
	Reaped int
	// Killed 是真正发出过 kill 的进程数。
	Killed int
	// Errors 是回收过程中的错误（杀不掉、状态写不进去等）。
	Errors []string
}

// Reap 回收过期且仍在跑的后台命令。
//
// 两条到期线（见 bgTaskExpired）：拥有者任务的 deadline，以及 TTL 兜底。谁先到算谁。
// 回收的动作是：按**进程组**杀掉进程（跨进程也成立，因为账本里存着 pid），然后把
// 那一行标成 lost —— 不是 failed：「它被回收了」和「它失败了」是两件事，agent 读到
// 这两个字时的下一步动作不一样。
func (r *BgTaskRegistry) Reap(ctx context.Context) (BgReapResult, error) {
	var result BgReapResult
	if r.store == nil {
		return result, errors.New("后台命令注册表未接线")
	}
	if err := r.store.EnsureBgTaskSchema(); err != nil {
		return result, err
	}
	rows, err := r.store.ListRunningBgTasks(ctx)
	if err != nil {
		return result, err
	}
	now := r.now()
	for _, row := range rows {
		expired, reason := bgTaskExpired(row, now, r.ttl)
		if !expired {
			continue
		}
		result.Examined++
		if row.PID > 0 && r.kill != nil {
			if err := r.kill(row.PID); err != nil {
				// 杀不掉也要把账本闭合：这一行留在 running 会被每一轮重复扫，
				// 而它已经过了保留期。错误被记进结果里，运维看得见。
				result.Errors = append(result.Errors,
					fmt.Sprintf("杀后台进程 %d 失败（%s）: %v", row.PID, row.SdkTaskID, err))
			} else {
				result.Killed++
			}
		}
		notice := fmt.Sprintf("后台命令被回收（%s）：%s\n输出文件：%s\n"+
			"（它被回收了，不是自己失败的；输出可能是残缺的。）", reason, bgShortCommand(row.Command), row.OutputPath)
		if err := r.store.UpdateBgTaskState(ctx, row.ID, db.BgTaskStateLost, nil, notice, now, false); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("标记后台命令 %d 为 lost 失败: %v", row.ID, err))
			continue
		}
		result.Reaped++
	}
	return result, nil
}

// bgTaskExpired 判定一条还在跑的后台命令是否该被回收。
//
// 两条独立的到期线：
//   - **任务 deadline**：拥有者任务到点了 ⇒ 这个任务的世界结束了 ⇒ 它的后台命令
//     不可能再被读（agent 那一轮早就结束了）。
//   - **TTL 兜底**：任务没有 deadline，或者任务还在跑，但这条命令已经活得比任何
//     合理的读取窗口都久了（默认 1 小时）。
//
// 判定只依赖入参 ⇒ 可以脱离数据库与活进程单测。
//
// 返回的第二个值是回收理由（写进账本的 notice），空串 = 没到期。
func bgTaskExpired(t db.BgTask, now time.Time, ttl time.Duration) (bool, string) {
	if t.DeadlineUnix > 0 && now.Unix() >= t.DeadlineUnix {
		return true, "任务已到 deadline"
	}
	if ttl > 0 {
		started := t.StartedAt
		if started.IsZero() {
			started = t.CreatedAt
		}
		if !started.IsZero() && !now.Before(started.Add(ttl)) {
			return true, fmt.Sprintf("超过 TTL（%s）", ttl)
		}
	}
	return false, ""
}

// StartJanitor 启动进程级回收者（幂等，只启动一次）。
//
// 为什么要有它：§11.7 说「进程级兜底 —— 无主任务且超过 TTL 的后台进程被回收」。
// 回收者必须一直有人跑：账本里 running 的行不会自己消失，没有 janitor 就等于
// 只有记账没有回收 —— 也就是进程泄漏。
//
// ctx 只用来停 janitor；默认用 context.Background()（进程活多久它跑多久）。
func (r *BgTaskRegistry) StartJanitor(ctx context.Context) {
	r.janitor.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		interval := bgTaskReapInterval()
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					res, err := r.Reap(context.WithoutCancel(ctx))
					if err != nil {
						log.Printf("[bg_task] 回收失败: %v", err)
						continue
					}
					if res.Reaped > 0 || len(res.Errors) > 0 {
						log.Printf("[bg_task] 回收：扫描 %d 条 running，回收 %d 条，杀进程 %d 个，错误 %d 条",
							res.Examined, res.Reaped, res.Killed, len(res.Errors))
					}
				}
			}
		}()
	})
}

// ---------- 完成提醒的投递 ----------

// dbBgTaskReminder 是生产环境的完成提醒投递口。
//
// 两条通道：
//  1. **探索图的 hint 节点**：任务的规划者每一轮都会读「本轮态势」里的 hints ⇒
//     提醒在**下一轮**就送到 agent 眼前。这是「agent 下一轮读得到」的主通道。
//  2. **任务邮箱（群聊）**：任务挂了群就把提醒投进 chat_group_messages。
//
// 通道 2 的读侧在本仓库里目前是断的（server/chat_group.go 的 WireGroupMailbox 从未被
// 调用，且聊天路径不带 RunInfo.TaskID）—— 那是 server/** 的地盘，本单元不改；
// 提醒照样写进去，等读侧接上就能用。
type dbBgTaskReminder struct {
	db *db.DB
}

// Deliver 投递一条完成提醒。
func (r *dbBgTaskReminder) Deliver(ctx context.Context, row db.BgTask, notice string) error {
	if r == nil || r.db == nil {
		return errors.New("提醒投递口未接线")
	}
	delivered := false
	var errs []string

	// 通道 1：探索图的 hint 节点。
	if expID := RunInfoFrom(ctx).ExplorationID; expID > 0 {
		payload := map[string]any{
			"text":        notice,
			"kind":        "background_cmd_done",
			"bg_task_id":  row.ID,
			"session_id":  row.SessionID,
			"sdk_task_id": row.SdkTaskID,
			"output_path": row.OutputPath,
			"state":       row.State,
		}
		if _, err := r.db.Exploration(expID).AddNode(db.KindHint, payload, 0, "active", "background_task", nil); err != nil {
			errs = append(errs, "写探索图 hint 失败: "+err.Error())
		} else {
			delivered = true
		}
	}

	// 通道 2：任务邮箱（群聊）。
	if row.OwnerTaskID > 0 {
		if group, err := r.db.GroupByTask(row.OwnerTaskID); err == nil && group != nil {
			taskID := row.OwnerTaskID
			if _, err := r.db.AddChatGroupMessage(&db.ChatGroupMessage{
				GroupID:   group.ID,
				FromAgent: "background_task",
				ToAgent:   "", // 广播给全群
				Kind:      "note",
				Body:      notice,
				TaskID:    &taskID,
			}); err != nil {
				errs = append(errs, "投任务邮箱失败: "+err.Error())
			} else {
				delivered = true
			}
		}
	}

	if !delivered {
		if len(errs) == 0 {
			return errors.New("没有可用的提醒投递通道（任务既没有探索图也没有群聊）")
		}
		return errors.New(strings.Join(errs, "；"))
	}
	if len(errs) > 0 {
		log.Printf("[bg_task] 部分提醒通道失败: %s", strings.Join(errs, "；"))
	}
	return nil
}
